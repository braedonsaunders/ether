package livetv

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBroadcastRequestCost(t *testing.T) {
	for _, mode := range []string{"order", "marathon", "personal"} {
		for _, n := range []int{10, 500} {
			t.Run(fmt.Sprintf("%s/%d", mode, n), func(t *testing.T) {
				now := time.Unix(12000, 0).UTC()
				s := testService(t, Options{Now: func() time.Time { return now }})
				s.o.DB.Exec(`CREATE TABLE resolver_availability (rating_key INTEGER PRIMARY KEY)`)
				s.o.Availability = sqlAvailability{s}
				c := testChannel()
				c.Items = nil
				c.Mode = mode
				if mode == "personal" {
					c.Mode = ""
					c.Personal = &PersonalRecipe{}
					s.o.Picker = personalTestPicker{}
				}
				for i := 0; i < n; i++ {
					c.Items = append(c.Items, Item{Key: core.MovieKey(int64(i + 1)), Title: "Title", DurationMS: 60000, Rating: "G"})
				}
				if _, err := s.SaveChannel(context.Background(), c); err != nil {
					t.Fatal(err)
				}
				u := core.User{ID: 1, IsOwner: true}
				ss := &session{ctx: context.Background(), user: u, ready: true, info: SessionInfo{ID: "cost", UserID: u.ID, Channel: c}}
				s.sessions["cost"] = ss
				tag := s.programmePolicy(context.Background(), Programme{Item: c.Items[0]})
				seg := transcode.Segment{PolicyTags: []any{tag}, Seq: 0, Start: now.Add(-12 * time.Second), Duration: 12 * time.Second}
				idx := costIndex{seg: seg}
				enc := &costEncoder{index: idx}
				originDone := make(chan struct{})
				close(originDone)
				b := &broadcast{renditions: map[string]*broadcastRendition{"cost": {encoder: enc, originDone: originDone, origin: seg.Start}}}
				ss.broadcast = &broadcastLease{broadcast: b, rendition: "cost"}
				for _, kind := range []string{"playlist", "segment", "dash playlist", "dash segment", "matroska boundary"} {
					ctx, stats := store.WithQueryStats(context.Background())
					start := time.Now()
					got, err := s.getSession(ctx, "cost", &u)
					if err != nil {
						t.Fatal(err)
					}
					r := httptest.NewRequest("GET", "/livetv/sessions/cost/index.m3u8", nil).WithContext(ctx)
					w := httptest.NewRecorder()
					if kind == "playlist" {
						s.serveBroadcastPlaylist(w, r, got)
					} else if kind == "dash playlist" {
						if err := s.serveBroadcastDASH(w, r, got); err != nil {
							t.Fatal(err)
						}
					} else if kind == "matroska boundary" {
						if !s.broadcastSegmentAllowed(ctx, got, seg) {
							t.Fatal("Matroska segment denied")
						}
					} else if kind == "dash segment" {
						s.serveBroadcastDASHPart(w, r, got, 0, 0, false)
					} else {
						s.serveBroadcastTS(w, r, got, 0)
					}
					if w.Code != 200 {
						t.Fatalf("status %d: %s", w.Code, w.Body.String())
					}
					if stats.Queries() > 2 {
						t.Fatalf("request scales with schedule: %d queries", stats.Queries())
					}
					for _, statement := range stats.Statements() {
						if strings.Contains(statement.SQL, "resolver_availability") {
							t.Fatal("availability on request")
						}
					}
					t.Logf("%s titles=%d queries=%d elapsed=%s status=%d", kind, n, stats.Queries(), time.Since(start), w.Code)
				}

				blocked := s.programmePolicy(context.Background(), Programme{Item: Item{Rating: "R"}})
				if !s.broadcastSegmentAllowed(context.Background(), ss, transcode.Segment{PolicyTags: []any{blocked}}) {
					t.Fatal("unrestricted viewer denied")
				}
				if err := s.SavePolicy(context.Background(), Policy{UserID: u.ID, MaxRating: "G"}); err != nil {
					t.Fatal(err)
				}
				if s.broadcastSegmentAllowed(context.Background(), ss, transcode.Segment{PolicyTags: []any{tag, blocked}}) {
					t.Fatal("cached decision survived policy change")
				}
				if !s.broadcastSegmentAllowed(context.Background(), ss, seg) {
					t.Fatal("permitted programme denied")
				}
				c.Enabled = false
				if _, err := s.SaveChannel(context.Background(), c); err != nil {
					t.Fatal(err)
				}
				if s.broadcastSegmentAllowed(context.Background(), ss, seg) {
					t.Fatal("cached decision survived channel change")
				}
				delete(s.sessions, "cost")
			})
		}
	}
}

type sqlAvailability struct{ s *Service }

func (a sqlAvailability) Available(ctx context.Context, key core.RatingKey) (bool, bool) {
	var x int
	a.s.o.DB.QueryRowContext(ctx, `SELECT rating_key FROM resolver_availability WHERE rating_key=?`, key).Scan(&x)
	return false, false
}

type costIndex struct {
	fakeBroadcastIndex
	seg transcode.Segment
}

func (i costIndex) Latest() (transcode.Segment, bool) { return i.seg, true }
func (i costIndex) Window(time.Time, time.Time) []transcode.Segment {
	return []transcode.Segment{i.seg}
}
func (i costIndex) Since(int64, int) []transcode.Segment { return []transcode.Segment{i.seg} }

type costEncoder struct {
	transcode.ContinuousEncoder
	index transcode.SegmentIndex
}

func (e *costEncoder) Segments() transcode.SegmentIndex { return e.index }

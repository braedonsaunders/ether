package plex

import (
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// All item surfaces must retain the ordered version identities first issued
// to this viewer/device, even before admission, through a refill, expiry,
// probe and start. PMS decisions are projections of the selected Media,
// rather than replacement version lists (see the recorded decision corpus).
func TestVersionIdentityAcrossPlayFlow(t *testing.T) {
	// Plex for LG and VIDAA hold one Auto Media instead (tv_app_queue_test.go).
	for _, product := range []string{"Plex for Samsung", "Plex for Android (TV)", "Plex Web"} {
		for _, count := range []int{0, 1, 3} {
			t.Run(product+"/"+string(rune('0'+count)), func(t *testing.T) {
				f, pb := newVariantPlaybackFixture(t)
				full := testVariantLists()[fightClub.Key]
				f.resolver.byKey[fightClub.Key] = full[:count]
				path := "/library/metadata/" + fightClub.Key.String()
				headers := []string{"X-Plex-Product", product, "Accept", "application/json"}
				media := func(stage string, r *httptest.ResponseRecorder) []wireMedia {
					t.Helper()
					if r.Code != http.StatusOK {
						t.Fatalf("%s: %d %s", stage, r.Code, r.Body.String())
					}
					// Hubs and queues can contain several titles. Locate this item's
					// Media recursively without depending on a particular shelf layout.
					var root any
					if err := json.Unmarshal(r.Body.Bytes(), &root); err != nil {
						t.Fatal(err)
					}
					var found []wireMedia
					var visit func(any)
					visit = func(v any) {
						switch x := v.(type) {
						case map[string]any:
							if x["ratingKey"] == fightClub.Key.String() {
								b, _ := json.Marshal(x["Media"])
								if err := json.Unmarshal(b, &found); err != nil {
									t.Fatal(err)
								}
								return
							}
							for _, child := range x {
								visit(child)
							}
						case []any:
							for _, child := range x {
								visit(child)
							}
						}
					}
					visit(root)
					if len(found) == 0 {
						t.Fatalf("%s: no media: %s", stage, r.Body.String())
					}
					return found
				}
				ids := func(ms []wireMedia) []flexID {
					out := make([]flexID, len(ms))
					for i, m := range ms {
						out[i] = m.ID
						if len(m.Part) != 1 || m.Part[0].ID != m.ID {
							t.Fatalf("invalid part: %+v", m)
						}
					}
					return out
				}
				hub := ids(media("hub", f.get(t, "/hubs/home/recentlyAdded", headers...)))
				if !reflect.DeepEqual(hub, []flexID{flexID(fightClub.Key)}) {
					t.Fatalf("hub projection: %v", hub)
				}
				if f.resolver.calls != 0 {
					t.Fatal("hub consulted variant resolver")
				}
				before := ids(media("pre-play", f.get(t, path+"?checkFiles=1&includeChapters=1", headers...)))
				wantCount := count
				if wantCount < 2 {
					wantCount = 1
				}
				if len(before) != wantCount {
					t.Fatalf("hub ids %v, want %d versions", before, wantCount)
				}
				check := func(stage, target string) {
					t.Helper()
					got := ids(media(stage, f.get(t, target, headers...)))
					if !reflect.DeepEqual(got, before) {
						t.Fatalf("%s ids %v, client holds %v", stage, got, before)
					}
				}
				// Search completes after pre-play has begun this flow.
				f.resolver.mu.Lock()
				f.resolver.byKey[fightClub.Key] = full
				f.resolver.mu.Unlock()
				check("pre-play repeat", path+"?checkFiles=1&includeChapters=1&includeMarkers=1")
				req := httptest.NewRequest(http.MethodPost, "/playQueues?type=video&uri="+urlEsc(path), nil)
				req.Header.Set("X-Plex-Token", ownerToken)
				req.Header.Set("X-Plex-Client-Identifier", "client-1")
				req.Header.Set("X-Plex-Product", product)
				req.Header.Set("Accept", "application/json")
				qr := httptest.NewRecorder()
				f.srv.ServeHTTP(qr, req)
				if got := ids(media("queue", qr)); !reflect.DeepEqual(got, before) {
					t.Fatalf("queue ids %v, client holds %v", got, before)
				}
				// Use the only valid selector for a singleton; new candidates must
				// not change its issued identity.
				query := "path=" + urlEsc(path) + "&mediaIndex=1&partIndex=9&session=flow339&hasMDE=1&directPlay=1&directStream=1&protocol=hls"
				if count < 2 {
					query = strings.Replace(query, "mediaIndex=1", "mediaIndex=0", 1)
				}
				dec := ids(media("decision", f.get(t, "/video/:/transcode/universal/decision?"+query, headers...)))
				selected := before[0]
				if count > 1 {
					selected = before[1]
				}
				if !reflect.DeepEqual(dec, []flexID{selected}) {
					t.Fatalf("decision ids %v, selected %d", dec, selected)
				}
				f.resolver.mu.Lock()
				f.resolver.byKey[fightClub.Key] = nil
				f.resolver.mu.Unlock()
				check("after decision and cache expiry", path)
				if r := f.get(t, "/video/:/transcode/universal/start.m3u8?"+query, headers...); r.Code != 200 {
					t.Fatalf("start: %d %s", r.Code, r.Body.String())
				}
				check("after start and probe", path+"?checkFiles=1&includeChapters=1")
				pb.mu.Lock()
				defer pb.mu.Unlock()
				if count < 2 && len(pb.variantCalls) != 0 {
					t.Fatalf("stale mediaIndex selected unissued release: %v", pb.variantCalls)
				}
				if count > 1 && (len(pb.variantCalls) == 0 || pb.variantCalls[0] != full[1].Release.ID) {
					t.Fatalf("wrong variant: %v", pb.variantCalls)
				}
			})
		}
	}
}

// Membership, not just hash-derived IDs, must survive a node switch. Node B
// deliberately has no candidate cache: recomputation cannot pass this check.
func TestVersionFlowSharedAcrossNodes(t *testing.T) {
	for _, count := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			a, b, _ := twoServers(t)
			pb := &fakeVariantPlayback{fakePlayback: newFakePlayback(sampleInfo)}
			ra := &fakeVariantResolver{byKey: testVariantLists()}
			full := ra.byKey[fightClub.Key]
			ra.byKey[fightClub.Key] = full[:count]
			rb := &fakeVariantResolver{byKey: map[core.RatingKey][]core.ReleaseVariant{}}
			// Both frontends route the fake source through one broker owner;
			// this test isolates version publication from encoder transport.
			a.srv.UsePlaybackBroker(b.srv.playbackBroker)
			a.srv.UsePlexStreams(b.srv.boundStreams)
			a.srv.d.Playback = pb
			a.srv.d.Variants = ra
			b.srv.d.Playback = pb
			b.srv.d.Variants = rb
			path := "/library/metadata/" + fightClub.Key.String()
			headers := []string{"Accept", "application/json", "X-Plex-Product", "Plex for Samsung"}
			fetch := func(f *fixture, stage, target string) []wireMedia {
				t.Helper()
				r := f.get(t, target, headers...)
				if r.Code != 200 {
					t.Fatalf("%s: %d %s", stage, r.Code, r.Body.String())
				}
				return decodeWireMedia(t, stage, r.Body)
			}
			ids := func(ms []wireMedia) []flexID {
				out := make([]flexID, len(ms))
				for i, m := range ms {
					out[i] = m.ID
				}
				return out
			}
			// An ordinary detail may offer versions but never pins a cold singleton.
			offered := ids(fetch(a, "detail", path))
			var rows int
			if err := a.srv.d.DB.QueryRow(`SELECT count(*) FROM plex_version_flows`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if count < 2 && rows != 0 {
				t.Fatal("ordinary singleton persisted a pin")
			}
			before := ids(fetch(b, "pre-play on B", path+"?checkFiles=1&includeChapters=1"))
			if !reflect.DeepEqual(before, offered) {
				t.Fatalf("B list %v, A issued %v", before, offered)
			}
			ra.mu.Lock()
			ra.byKey[fightClub.Key] = []core.ReleaseVariant{full[0], full[2], full[1]}
			ra.mu.Unlock()
			// Queue comes back to A after candidates have changed.
			req := httptest.NewRequest(http.MethodPost, "/playQueues?type=video&uri="+urlEsc(path), nil)
			req.Header.Set("X-Plex-Token", ownerToken)
			req.Header.Set("X-Plex-Client-Identifier", "client-1")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("X-Plex-Product", "Plex for Samsung")
			qr := httptest.NewRecorder()
			a.srv.ServeHTTP(qr, req)
			if qr.Code != 200 {
				t.Fatalf("queue: %d %s", qr.Code, qr.Body.String())
			}
			if got := ids(decodeWireMedia(t, "queue", qr.Body)); !reflect.DeepEqual(got, before) {
				t.Fatalf("queue %v, held %v", got, before)
			}
			query := "path=" + urlEsc(path) + "&mediaIndex=1&session=ha339&directPlay=1&directStream=1&protocol=hls"
			if count < 2 {
				query = strings.Replace(query, "mediaIndex=1", "mediaIndex=0", 1)
			}
			dec := ids(fetch(b, "decision on B", "/video/:/transcode/universal/decision?"+query))
			selected := before[0]
			if count > 1 {
				selected = before[1]
			}
			if !reflect.DeepEqual(dec, []flexID{selected}) {
				t.Fatalf("decision %v, want %d", dec, selected)
			}
			if r := a.get(t, "/video/:/transcode/universal/start.m3u8?"+query, headers...); r.Code != 200 {
				t.Fatalf("start %d %s", r.Code, r.Body.String())
			}
			if got := ids(fetch(b, "metadata after start", path)); !reflect.DeepEqual(got, before) {
				t.Fatalf("after start %v, held %v", got, before)
			}
			// A terminal timeline releases the shared flow, including one opened on B.
			b.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&state=stopped&time=1000", headers...)
			if err := a.srv.d.DB.QueryRow(`SELECT count(*) FROM plex_version_flows`).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("stop left %d rows: %v", rows, err)
			}
			if got := fetch(a, "picker after stop", path); len(got) != 3 {
				t.Fatalf("picker stayed frozen after stop: %d", len(got))
			}
		})
	}
}

func TestVersionFlowPendingExpiry(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	full := f.resolver.byKey[fightClub.Key]
	f.resolver.byKey[fightClub.Key] = nil
	path := "/library/metadata/" + fightClub.Key.String()
	first := f.get(t, path+"?checkFiles=1", "Accept", "application/json")
	if got := decodeWireMedia(t, "cold pre-play", first.Body); len(got) != 1 {
		t.Fatal("cold pre-play must be Auto")
	}
	f.resolver.mu.Lock()
	f.resolver.byKey[fightClub.Key] = full
	f.resolver.mu.Unlock()
	f.srv.d.Now = func() time.Time { return fixedNow.Add(versionPreplayTTL + time.Second) }
	next := f.get(t, path, "Accept", "application/json")
	if got := decodeWireMedia(t, "abandoned pre-play", next.Body); len(got) != 3 {
		t.Fatal("abandoned pre-play hid picker")
	}
}

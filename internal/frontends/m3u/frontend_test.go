package m3u

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestGuideFiltersChannelsAndEscapesXML(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "valid" {
			return nil, errors.New("bad grant")
		}
		return &frontend.Viewer{ID: 7}, nil
	}}
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	live := frontendtest.LiveTV{
		ChannelsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.LiveChannel, error) {
			if v.ID != 7 {
				t.Fatal("wrong viewer")
			}
			return []frontend.LiveChannel{{ID: "one", Name: "A & B"}}, nil
		},
		GuideFunc: func(_ context.Context, v *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
			if v.ID != 7 || q.Device.ID == "" || q.Device.Product != "M3U" || q.Until.Sub(q.From) > 7*24*time.Hour {
				t.Fatal("wrong guide scope")
			}
			return []frontend.LiveProgramme{{ChannelID: "one", Title: "Film <One>", Start: start, End: start.Add(time.Hour), Season: 2, Episode: 3}, {ChannelID: "hidden", Title: "Hidden", Start: start, End: start.Add(time.Hour)}}, nil
		},
	}
	var seen []string
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: identity, LiveTV: live, Sessions: frontendtest.Sessions{}, ClientSeen: func(id string) { seen = append(seen, id) }})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token  string
		status int
	}{{"valid", 200}, {"invalid", 401}} {
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/m3u/"+tc.token+"/guide.xml", nil))
		if w.Code != tc.status {
			t.Fatal(w.Code)
		}
		if tc.status == 200 && (!strings.Contains(w.Body.String(), "A &amp; B") || !strings.Contains(w.Body.String(), "Film &lt;One&gt;") || strings.Contains(w.Body.String(), "Hidden") || !strings.Contains(w.Body.String(), "1.2.")) {
			t.Fatal(w.Body.String())
		}
	}
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "m3u-") {
		t.Fatalf("seen clients: %+v", seen)
	}
}

func TestGuideSplitsCappedResultsByChannel(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	calls := 0
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, Sessions: frontendtest.Sessions{}, LiveTV: frontendtest.LiveTV{
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{{ID: "one", Name: "One"}, {ID: "two", Name: "Two"}}, nil
		},
		GuideFunc: func(_ context.Context, _ *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
			calls++
			if len(q.Channels) == 2 {
				return make([]frontend.LiveProgramme, q.Limit), nil
			}
			if len(q.Channels) != 1 {
				t.Fatalf("channels %+v", q.Channels)
			}
			return []frontend.LiveProgramme{{ID: q.Channels[0], ChannelID: q.Channels[0], Title: q.Channels[0], Start: q.From, End: q.From.Add(time.Hour)}}, nil
		},
	}}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m3u/grant/guide.xml", nil))
	if rec.Code != 200 || calls != 3 || !strings.Contains(rec.Body.String(), `<title>one</title>`) || !strings.Contains(rec.Body.String(), `<title>two</title>`) {
		t.Fatalf("guide status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

func TestGuideDeduplicatesProgrammesAcrossTimeSplits(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := frontend.LiveProgramme{ID: "crossing", ChannelID: "one", Title: "Film", Start: start, End: start.Add(2 * time.Hour)}
	i := instance{host: frontend.Host{LiveTV: frontendtest.LiveTV{GuideFunc: func(_ context.Context, _ *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
		if q.Until.Sub(q.From) > time.Hour {
			return []frontend.LiveProgramme{p, p}, nil
		}
		return []frontend.LiveProgramme{p}, nil
	}}}}
	got, err := i.collectGuide(context.Background(), &frontend.Viewer{ID: 7}, frontend.LiveGuideQuery{Channels: []string{"one"}, From: start, Until: start.Add(2 * time.Hour), Limit: 2})
	if err != nil || len(got) != 1 || got[0].ID != "crossing" {
		t.Fatalf("programmes=%+v err=%v", got, err)
	}
}

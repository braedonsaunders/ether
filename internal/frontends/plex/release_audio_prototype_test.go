package plex

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Protocol smoke check for the prototype; real Plex client behavior is still
// the question this experiment must answer.
func TestReleaseAudioPrototype(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	enabled := false
	baseConfig := f.srv.testConfig()
	f.srv.setTestConfig(func() config.Config {
		c := baseConfig()
		c.Plex.ReleaseAudioSelector = enabled
		return c
	})
	metadata := "/library/metadata/" + fightClub.Key.String()
	if strings.Contains(f.get(t, metadata).Body.String(), "Release:") {
		t.Fatal("prototype is not off by default")
	}
	enabled = true
	body := f.get(t, metadata).Body.String()
	var doc struct {
		Video []struct {
			Media []struct {
				Part []struct {
					Stream []struct {
						ID    int64  `xml:"id,attr"`
						Title string `xml:"title,attr"`
					} `xml:"Stream"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, st := range doc.Video[0].Media[0].Part[0].Stream {
		if strings.HasPrefix(st.Title, "Release:") {
			ids[st.Title] = st.ID
		}
	}
	if len(ids) != 3 {
		t.Fatalf("expected auto and two releases: %v", ids)
	}
	pick := func(id int64) int {
		req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/library/parts/%s?audioStreamID=%d", fightClub.Key, id), nil)
		req.RemoteAddr = "192.168.1.50:51000"
		req.Header.Set("X-Plex-Token", ownerToken)
		req.Header.Set("X-Plex-Client-Identifier", "client-1")
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		return rec.Code
	}
	id := ids["Release: 4K Dolby Vision Remux"]
	if code := pick(id); code != http.StatusOK {
		t.Fatalf("release select: %d", code)
	}
	decision := "/video/:/transcode/universal/decision?path=" + metadata
	rec := f.get(t, decision+"&session=release-menu&offset=123")
	if rec.Code != 200 || len(pb.variantCalls) != 1 || pb.variantCalls[0] != "rel-4k" {
		t.Fatalf("release not selected: calls=%v response=%s", pb.variantCalls, rec.Body.String())
	}
	// Explicit audioStreamID works without a preceding part selection too.
	rec = f.get(t, decision+fmt.Sprintf("&session=release-query&audioStreamID=%d", ids["Release: 720p Blu-ray (High Compatibility)"]))
	if rec.Code != 200 || pb.variantCalls[len(pb.variantCalls)-1] != "rel-compat" {
		t.Fatalf("query release not selected: %v / %s", pb.variantCalls, rec.Body.String())
	}
	if code := pick(ids["Release: Auto"]); code != 200 {
		t.Fatalf("auto select: %d", code)
	}
	before := len(pb.variantCalls)
	rec = f.get(t, decision+"&session=release-auto")
	if rec.Code != 200 || len(pb.variantCalls) != before || pb.prepared == 0 {
		t.Fatalf("auto did not restore automatic resolution: %s", rec.Body.String())
	}
	enabled = false
	if code := pick(id); code != 400 {
		t.Fatalf("disabled prototype accepted virtual stream: %d", code)
	}
	if strings.Contains(f.get(t, metadata).Body.String(), "Release:") {
		t.Fatal("disabled prototype still advertises releases")
	}
}

type releaseAudioBudgetPlayback struct {
	*fakeVariantPlayback
	preflights []string
}

func (p *releaseAudioBudgetPlayback) PrepareWithin(_ context.Context, _ time.Duration, _ *core.User, _ core.RatingKey, _ core.ClientCaps, release string, _ bool) (*core.PlaySession, error) {
	p.preflights = append(p.preflights, release)
	if release == "" {
		return nil, core.ErrSourceUnavailable
	}
	return &core.PlaySession{}, nil
}

func TestReleaseAudioPrototypeDoesNotPreflightAuto(t *testing.T) {
	pb := &releaseAudioBudgetPlayback{fakeVariantPlayback: &fakeVariantPlayback{fakePlayback: newFakePlayback(sampleInfo)}}
	f := newVariantFixture(t, pb)
	baseConfig := f.srv.testConfig()
	f.srv.setTestConfig(func() config.Config {
		c := baseConfig()
		c.Plex.ReleaseAudioSelector = true
		return c
	})
	f.get(t, "/library/metadata/"+fightClub.Key.String())
	f.srv.selections.update(ownerUser.ID, int64(fightClub.Key), func(sel *selection) {
		sel.releaseAudio = "rel-4k"
	})
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&session=release-preflight")
	if rec.Code != 200 || len(pb.variantCalls) != 1 || pb.variantCalls[0] != "rel-4k" {
		t.Fatalf("selected release was blocked by Auto: preflights=%v calls=%v response=%s", pb.preflights, pb.variantCalls, rec.Body.String())
	}
	for _, release := range pb.preflights {
		if release == "" {
			t.Fatal("explicit release unnecessarily prepared Auto")
		}
	}
}

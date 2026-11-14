package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type sessionSubtitleDownload struct {
	*fakeSubtitles
	entered chan string
	release chan struct{}
}

func (s *sessionSubtitleDownload) DownloadSubtitle(ctx context.Context, title *core.Title, user *core.User, id int64) (core.StreamInfo, error) {
	client := ""
	if pr := FromContext(ctx); pr != nil {
		client = pr.ClientID
	}
	s.entered <- client
	select {
	case <-s.release:
		return s.fakeSubtitles.DownloadSubtitle(ctx, title, user, id)
	case <-ctx.Done():
		return core.StreamInfo{}, ctx.Err()
	}
}

// A search pick crosses an asynchronous download, a metadata poll and a
// version restart. The exact stream must remain attached to the same Part
// throughout, including when another sidecar already covers that language.
func TestSubtitleVersionPickIdentity(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	old := sidecar("nl", "ether-subtitle:old")
	pb.info.Streams = append(append([]core.StreamInfo(nil), pb.info.Streams...), old)
	subs := &sessionSubtitleDownload{newFakeSubtitles(), make(chan string, 1), make(chan struct{})}
	subs.offers = []SubtitleOffer{{ID: 101, Language: "nl"}}
	f.srv.setSubtitles(subs)
	headers := []string{"X-Plex-Product", "Plex for Android (TV)", "Accept", "application/json"}
	path := "/library/metadata/" + fightClub.Key.String()
	before := decodeWireMedia(t, "before", f.get(t, path, headers...).Body)
	decision := "/video/:/transcode/universal/decision?path=" + path + "&mediaIndex=1&session=sub-version"
	if rec := f.get(t, decision, headers...); rec.Code != 200 {
		t.Fatalf("decision: %d %s", rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, f.get(t, path+"/subtitles?language=nl", headers...))
	key := mc["Stream"].([]any)[0].(map[string]any)["key"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPut, path+"/subtitles?key="+url.QueryEscape(key), nil).WithContext(ctx)
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	cancel() // The response is finished; the download must keep its identity.
	if rec.Code != 200 {
		t.Fatalf("pick: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case client := <-subs.entered:
		if client != "client-1" {
			t.Fatalf("download lost playback client: %q", client)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted pick did not start")
	}
	close(subs.release)
	pickedID := flexID(externalID(fightClub.Key, "ether-subtitle:pick-101"))
	check := func(stage string) {
		t.Helper()
		after := decodeWireMedia(t, stage, f.get(t, path, headers...).Body)
		if len(after) != len(before) {
			t.Fatalf("%s: version count changed", stage)
		}
		found := false
		for i, m := range after {
			if m.ID != before[i].ID || len(m.Part) != 1 || m.Part[0].ID != before[i].Part[0].ID || m.Part[0].Key != before[i].Part[0].Key {
				t.Fatalf("%s: version %d changed identity: %+v", stage, i, m)
			}
			for _, st := range m.Part[0].Stream {
				if st.ID == pickedID {
					if i != 1 || !bool(st.Selected) {
						t.Fatalf("%s: pick attached to version %d selected=%v", stage, i, st.Selected)
					}
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("%s: exact downloaded stream missing", stage)
		}
	}
	check("poll")
	req = httptest.NewRequest(http.MethodPut, fmt.Sprintf("/library/parts/%d?subtitleStreamID=%d", before[1].Part[0].ID, pickedID), nil)
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("part selection: %d %s", rec.Code, rec.Body.String())
	}
	check("selected")
	if rec := f.get(t, decision+fmt.Sprintf("&subtitleStreamID=%d", pickedID), headers...); rec.Code != 200 {
		t.Fatalf("restart decision: %d %s", rec.Code, rec.Body.String())
	}
	check("restart")
}

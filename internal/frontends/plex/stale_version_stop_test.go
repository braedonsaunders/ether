package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A subtitle/version switch reuses the public player session, but a late
// stopped timeline still names the old encoder. It must not revoke the new
// decision's authorization, even when the first new start was cancelled.
func TestVersionRestartSurvivesOldTimelineStop(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	subs := newFakeSubtitles()
	subs.offers = []SubtitleOffer{{ID: 101, Language: "nl"}}
	f.srv.setSubtitles(subs)
	headers := []string{"X-Plex-Session-Identifier", "shared-player", "X-Plex-Product", "Plex for Android (TV)", "Accept", "application/json"}
	path := "/library/metadata/" + fightClub.Key.String()
	f.get(t, path, headers...)
	query := "?path=" + path + "&protocol=hls&directPlay=0&directStream=1"
	request := func(method, target string, ctx context.Context) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, nil).WithContext(ctx)
		req.Header.Set("X-Plex-Token", ownerToken)
		req.Header.Set("X-Plex-Client-Identifier", "client-1")
		for i := 0; i < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		return rec
	}
	get := func(target string) {
		t.Helper()
		if rec := f.get(t, target, headers...); rec.Code != 200 {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
	get("/video/:/transcode/universal/decision" + query + "&mediaIndex=1&session=old-encoder")
	get("/video/:/transcode/universal/start.m3u8" + query + "&mediaIndex=1&session=old-encoder")
	mc := decodeJSON(t, f.get(t, path+"/subtitles?language=nl", headers...))
	offer := mc["Stream"].([]any)[0].(map[string]any)["key"].(string)
	if rec := request(http.MethodPut, path+"/subtitles?key="+url.QueryEscape(offer), context.Background()); rec.Code != 200 {
		t.Fatalf("subtitle pick: %d", rec.Code)
	}
	f.srv.subtitlePicks.wait(context.Background(), ownerUser.ID, fightClub.Key)
	get("/video/:/transcode/universal/decision" + query + "&mediaIndex=2&session=new-encoder")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request(http.MethodGet, "/video/:/transcode/universal/start.m3u8"+query+"&mediaIndex=2&session=new-encoder", ctx)
	get("/:/timeline?ratingKey=" + fightClub.Key.String() + "&state=stopped&time=1000&session=old-encoder&mediaIndex=1")
	pr := &Request{User: &ownerUser, ClientID: "client-1", SessionID: "shared-player"}
	armed := f.srv.decisionArmed(pr, core.RatingKey(fightClub.Key))
	rec := f.get(t, "/video/:/transcode/universal/start.m3u8"+query+"&mediaIndex=2&session=new-encoder", headers...)
	if !armed || rec.Code != 200 {
		t.Fatalf("old stop revoked new decision: armed=%v start=%d body=%s", armed, rec.Code, rec.Body.String())
	}
}

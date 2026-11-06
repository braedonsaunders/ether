package plex

import (
	"net/url"
	"testing"
)

// Probe the measured permissive selector grammar independently of the client
// profile. A single-Part source ignores partIndex, while malformed mediaIndex
// falls back to its first Media. Explicit unavailable versions remain refusals.
func TestPMSSinglePartDecisionSelectorGrammar(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, media := range []string{"", "abc", "-1", "0"} {
		for _, part := range []string{"", "abc", "-1", "9"} {
			q := url.Values{"path": {"/library/metadata/" + fightClub.Key.String() + "?ignored=1"}, "mediaIndex": {media}, "partIndex": {part}, "directPlay": {"1"}}
			rec := f.get(t, "/video/:/transcode/universal/decision?"+q.Encode(), jsonAccept...)
			if rec.Code != 200 {
				t.Fatalf("media=%q part=%q: %d %s", media, part, rec.Code, rec.Body.String())
			}
			mc := decodeJSON(t, rec)
			md := mc["Metadata"].([]any)[0].(map[string]any)
			if md["summary"] == nil {
				t.Fatalf("decision lost summary: %v", md)
			}
			if _, ok := md["Image"]; ok {
				t.Fatal("decision included Image")
			}
			if _, ok := md["Guid"]; ok {
				t.Fatal("decision included Guid")
			}
			if len(md["Media"].([]any)) != 1 {
				t.Fatalf("decision media: %v", md["Media"])
			}
		}
	}
}

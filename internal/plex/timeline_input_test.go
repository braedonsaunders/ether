package plex

import (
	"net/http"
	"testing"
)

func TestMalformedTimelineDoesNotWriteState(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, suffix := range []string{
		"time=1", "state=unknown&time=1", "state=playing&time=abc", "state=stopped&time=-1",
		"state=playing&time=NaN", "state=playing&time=1&duration=", "state=playing&duration=-1",
		"state=playing&duration=999999999999999999",
	} {
		rec := f.get(t, "/:/timeline?ratingKey="+fightClub.Key.String()+"&"+suffix)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", suffix, rec.Code)
		}
	}
	if len(f.state.progress) != 0 || len(f.srv.sessions.list()) != 0 {
		t.Fatal("malformed timeline changed watch state or sessions")
	}
	for _, value := range []string{"", "abc", "NaN"} {
		rec := f.get(t, "/:/progress?key="+fightClub.Key.String()+"&identifier=com.plexapp.plugins.library&time="+value)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("progress time=%q: %d", value, rec.Code)
		}
	}
	if len(f.state.progress) != 0 {
		t.Fatal("malformed progress changed watch state")
	}
}

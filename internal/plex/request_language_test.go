package plex

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrackLocaleNegotiation(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"de;q=0.1,fr;q=0.9", "fr"}, {"de;q=0", "en"}, {"zzz,de", "de"}, {"*,de", "de"}, {"de,fr", "de"}, {"de;q=0.5,fr;q=0.5", "de"}, {"ca,fr;q=0.1", "fr"}, {"nn,nb;q=0.1", "nb"}, {"ca,zh-TW", "ca"}, {"zh-Hant-TW,de", "de"}, {"de,,fr", "de"},
		{"de;q=abc,fr", "en"}, {"de;q=.5,fr", "en"}, {"de;q=00.5,fr", "en"}, {"de;q=+0.5,fr", "en"}, {"de;q=-1,fr", "en"}, {"de;q=1.5,fr", "en"}, {"de;q=,fr", "en"}, {"de;Q=0.9,fr", "en"}, {"de;x=1,fr", "en"}, {"de;q=0.5;x=1,fr", "en"}, {";de,fr", "en"},
	} {
		if got := chooseRequestLanguage(tc.raw, true); got != tc.want {
			t.Errorf("%s: %s want %s", tc.raw, got, tc.want)
		}
	}
	for _, query := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/identity", nil)
		r.Header.Set("Accept-Language", "fr")
		if query {
			r.URL.RawQuery = "X-Plex-Accept-Language=de"
		} else {
			r.Header.Set("X-Plex-Accept-Language", "de")
		}
		if got := ParseRequest(r).Language; got != "de" {
			t.Fatal(got)
		}
		r.Header.Set("X-Plex-Language", "zh-TW")
		if got := ParseRequest(r).Language; got != "zh-TW" {
			t.Fatal(got)
		}
	}
	f := newFixture()
	defer f.srv.Close()
	f.playback.info = probedInfo
	path := "/library/metadata/" + fightClub.Key.String()
	rec := f.get(t, path, "X-Plex-Accept-Language", "de", "Accept-Language", "fr")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `language="Französisch"`) {
		t.Fatalf("HTTP locale %d %s", rec.Code, rec.Body.String())
	}
	rec = f.get(t, path, "X-Plex-Language", "fr", "X-Plex-Accept-Language", "de")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `language="français"`) {
		t.Fatalf("HTTP precedence %d %s", rec.Code, rec.Body.String())
	}
}

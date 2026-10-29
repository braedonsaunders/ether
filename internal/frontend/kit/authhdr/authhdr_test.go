package authhdr

import (
	"net/http/httptest"
	"testing"
)

func TestMediaBrowser(t *testing.T) {
	p, err := MediaBrowser(`MediaBrowser Client="TV%20App", Device="Living, room", Token="a\"b", token=lower`)
	if err != nil || p["Client"] != "TV App" || p["Device"] != "Living, room" || p["Token"] != `a"b` || p["token"] != "lower" {
		t.Fatal(p, err)
	}
	for _, v := range []string{`MediaBrowser A="unterminated`, `MediaBrowser A=x,`, `MediaBrowser A=%zz`, `MediaBrowser A=x,A=y`, `Bearer a`} {
		if _, err := MediaBrowser(v); err == nil {
			t.Fatalf("accepted %q", v)
		}
	}
}
func TestHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/?api_key=query", nil)
	r.Header.Set("X-Emby-Token", "header")
	r.Header.Set("X-Plex-Token", "plex")
	r.Header.Set("X-Other", "ignored")
	if got := MediaBrowserToken(r); got != "header" {
		t.Fatal(got)
	}
	p := Plex(r.Header)
	if p.Get("X-Plex-Token") != "plex" || p.Get("X-Other") != "" {
		t.Fatal(p)
	}
	p.Set("X-Plex-Token", "changed")
	if r.Header.Get("X-Plex-Token") != "plex" {
		t.Fatal("alias")
	}
	if v, ok := Bearer("bearer secret"); !ok || v != "secret" {
		t.Fatal(v, ok)
	}
	if _, ok := Bearer("Bearer a b"); ok {
		t.Fatal("accepted malformed bearer")
	}
}
func FuzzMediaBrowser(f *testing.F) {
	f.Add(`MediaBrowser Client="TV", Token="abc"`)
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) { _, _ = MediaBrowser(s) })
}

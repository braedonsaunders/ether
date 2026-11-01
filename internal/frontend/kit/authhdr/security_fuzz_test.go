package authhdr

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func FuzzAuthHeaders(f *testing.F) {
	f.Add(`MediaBrowser Token="secret", DeviceId="tv"`)
	f.Add(`Emby Token="a%2Cb", Token="duplicate"`)
	f.Add(`Bearer token`)
	f.Fuzz(func(t *testing.T, value string) {
		a, e := MediaBrowser(value)
		b, e2 := MediaBrowser(value)
		if (e == nil) != (e2 == nil) || !reflect.DeepEqual(a, b) {
			t.Fatal("nondeterministic parser")
		}
		if token, ok := Bearer(value); ok && (token == "" || strings.ContainsAny(token, " \r\n\t")) {
			t.Fatal("bad bearer")
		}
		for _, key := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"} {
			r := &http.Request{Header: http.Header{}, URL: &url.URL{}}
			r.Header.Set(key, value)
			_ = MediaBrowserToken(r)
		}
		h := http.Header{"X-Plex-Token": {value}, "X-Plex-Product": {value}, "Other": {value}}
		result := Plex(h)
		if len(result) != 2 || result.Get("Other") != "" {
			t.Fatal("header isolation")
		}
		result["X-Plex-Token"][0] = "changed"
		if h.Get("X-Plex-Token") != value {
			t.Fatal("aliased header")
		}
	})
}

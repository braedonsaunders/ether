package plex

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func FuzzPlexRequestHeaders(f *testing.F) {
	f.Add("token", "Plex for TV", "12", "0", "25")
	f.Add("", "\r\n", "", "-1", "99999999999999999999999")
	f.Fuzz(func(t *testing.T, token, product, client, start, size string) {
		r := httptest.NewRequest("GET", "/library/sections", nil)
		r.Header.Set("X-Plex-Token", token)
		r.Header.Set("X-Plex-Product", product)
		r.Header.Set("X-Plex-Client-Identifier", client)
		r.Header.Set("X-Plex-Container-Start", start)
		r.Header.Set("X-Plex-Container-Size", size)
		parsed := ParseRequest(r)
		if parsed.Token != strings.TrimSpace(token) || parsed.Product != strings.TrimSpace(product) || parsed.ClientID != strings.TrimSpace(client) || parsed.Start < 0 {
			t.Fatal("header parsing changed")
		}
	})
}

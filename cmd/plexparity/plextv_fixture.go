package main

import (
	"io"
	"net/http"
)

// plextvFixture simulates an entitled owner exclusively inside the offline
// test network. Production entitlement validation is unchanged.
func plextvFixture() http.Handler { return trackedPlexTVFixture(nil) }

func trackedPlexTVFixture(tracker *fixtureTracker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "parity-owner" && r.URL.Query().Get("auth_token") != "parity-owner" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v2/user":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":1,"username":"owner","title":"Owner","subscription":{"active":true,"status":"active"}}`)
		case "/api/v2/features":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `[]`)
		case "/api/v2/server/users/subscriptions":
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<MediaContainer><user id="1"><subscription active="1"/></user></MediaContainer>`)
		default:
			if tracker != nil {
				tracker.record(r.URL.Path)
			}
			http.NotFound(w, r)
		}
	})
}

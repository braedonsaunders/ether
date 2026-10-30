//go:build plexparity && conformlab

package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type labPrimaryCleanup struct {
	Before, Logout, After, ServiceHealth int
	Verified                             bool
}

// ServiceHealth is anonymous server health, not an independent authenticated
// control. The same primary token must work before logout and be denied after.
func (ref labReferenceData) logoutPrimaryProof() labPrimaryCleanup {
	result := labPrimaryCleanup{}
	if ref.token == "" || ref.device == "" {
		return result
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path string, authenticated bool) int {
		req, err := http.NewRequest(method, ref.base+path, nil)
		if err != nil {
			return 0
		}
		if authenticated {
			req.Header.Set("X-Emby-Token", ref.token)
			req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="`+ref.device+`", Version="1", Token="`+ref.token+`"`)
		}
		response, err := client.Do(req)
		if err != nil {
			return 0
		}
		response.Body.Close()
		return response.StatusCode
	}
	result.Before = request("GET", "/Users/Me", true)
	// Attempt owned logout even if the precheck fails.
	result.Logout = request("POST", "/Sessions/Logout", true)
	result.After = request("GET", "/Users/Me", true)
	result.ServiceHealth = request("GET", "/System/Info/Public", false)
	result.Verified = result.Before == 200 && result.Logout >= 200 && result.Logout < 300 && result.After == 401 && result.ServiceHealth == 200
	return result
}

type labPlaybackStatusWriter struct {
	http.ResponseWriter
	status, bytes int
}

func (w *labPlaybackStatusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *labPlaybackStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *labPlaybackStatusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *labPlaybackStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func TestLabPrimaryCleanupProof(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		before, logout, after, health int
		transport                     bool
		verified                      bool
	}{
		{"success", 200, 204, 401, 200, false, true},
		{"token-still-valid", 200, 204, 200, 200, false, false},
		{"unhealthy-service", 200, 204, 401, 503, false, false},
		{"precheck-denied", 401, 204, 401, 200, false, false},
		{"logout-failed", 200, 500, 401, 200, false, false},
		{"transport-failure", 200, 204, 401, 200, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				paths := []string{"/Users/Me", "/Sessions/Logout", "/Users/Me", "/System/Info/Public"}
				methods := []string{"GET", "POST", "GET", "GET"}
				if calls > 4 {
					t.Error("unexpected extra request")
					w.WriteHeader(500)
					return
				}
				if r.URL.Path != paths[calls-1] || r.Method != methods[calls-1] {
					t.Error("wrong proof sequence")
				}
				token, device, err := labRequestIdentity(r)
				if calls == 4 {
					if token != "" || device != "" {
						t.Error("service health must be anonymous")
					}
				} else if err != nil || token != "primary-token" || device != "owned-primary" {
					t.Error("exact owned credential not preserved")
				}
				if tc.transport && calls == 2 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				w.WriteHeader([]int{tc.before, tc.logout, tc.after, tc.health}[calls-1])
			}))
			defer server.Close()
			result := (labReferenceData{base: server.URL, token: "primary-token", device: "owned-primary", session: "owned-session"}).logoutPrimaryProof()
			wantLogout := tc.logout
			if tc.transport {
				wantLogout = 0
			}
			if result.Verified != tc.verified || result.Before != tc.before || result.Logout != wantLogout || result.After != tc.after || result.ServiceHealth != tc.health || calls != 4 {
				t.Fatalf("unexpected nonsecret receipt: %+v calls=%d", result, calls)
			}
		})
	}
}

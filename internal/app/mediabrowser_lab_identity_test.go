//go:build plexparity && conformlab

package app

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The pinned Emby SDK contract specifies GET /Users/{Id}, not /Users/Me.
// Only the GUID returned by this credential's own login may select its route.
func labValidUserID(id string) bool {
	if len(id) == 36 {
		if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			return false
		}
		id = strings.ReplaceAll(id, "-", "")
	}
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (ref labReferenceData) protectedPath() string {
	if ref.protocol != "emby" {
		return "/Users/Me"
	}
	if !labValidUserID(ref.user) {
		return ""
	}
	return "/Users/" + ref.user
}

func labOwnedRequest(ref labReferenceData, path, method string) (*http.Request, error) {
	req, err := http.NewRequest(method, ref.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Token", ref.token)
	req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="`+ref.device+`", Version="1", Token="`+ref.token+`"`)
	return req, nil
}

func labOwnedStatus(client *http.Client, ref labReferenceData, path, method string) int {
	req, err := labOwnedRequest(ref, path, method)
	if err != nil {
		return 0
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Return the actual status separately from identity validity. A 200 for a
// different/missing UserDto.Id cannot establish authenticated health.
func (ref labReferenceData) protectedCheck(client *http.Client) (int, bool) {
	path := ref.protectedPath()
	if path == "" {
		return 0, false
	}
	req, err := labOwnedRequest(ref, path, "GET")
	if err != nil {
		return 0, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return resp.StatusCode, false
	}
	if ref.protocol != "emby" {
		return resp.StatusCode, true
	}
	var user struct{ Id string }
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user)
	return resp.StatusCode, err == nil && labValidUserID(user.Id) && strings.EqualFold(user.Id, ref.user)
}

func TestLabEmbyOwnedIdentityCleanup(t *testing.T) {
	const user = "0123456789abcdef0123456789abcdef"
	const foreign = "fedcba9876543210fedcba9876543210"
	for _, mode := range []string{"primary", "probe"} {
		for _, scenario := range []string{"success", "still-valid", "missing-user", "invalid-user", "mismatched-response", "missing-response"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				loggedOut := false
				protected, logouts, anonymous := 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					token, device, err := labRequestIdentity(r)
					if err != nil {
						t.Error("ambiguous identity")
					}
					if r.URL.Path == "/System/Info/Public" {
						anonymous++
						if token != "" || device != "" {
							t.Error("anonymous check received credential")
						}
						w.WriteHeader(200)
						return
					}
					if token != "owned" && token != "control" {
						t.Error("unexpected credential")
					}
					if device != token+"-device" {
						t.Error("device not bound to credential")
					}
					if r.URL.Path == "/Sessions/Logout" && r.Method == "POST" {
						if token != "owned" {
							t.Error("control logged out")
						}
						logouts++
						loggedOut = true
						w.WriteHeader(204)
						return
					}
					if r.URL.Path != "/Users/"+user || r.Method != "GET" {
						t.Error("incorrect protected route")
						w.WriteHeader(500)
						return
					}
					protected++
					if token == "owned" && loggedOut && scenario != "still-valid" {
						w.WriteHeader(401)
						return
					}
					id := user
					if scenario == "mismatched-response" {
						id = foreign
					}
					if scenario == "missing-response" {
						id = ""
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
				}))
				defer server.Close()
				owned := labReferenceData{base: server.URL, token: "owned", device: "owned-device", protocol: "emby", user: user}
				control := labReferenceData{base: server.URL, token: "control", device: "control-device", protocol: "emby", user: user}
				if scenario == "missing-user" {
					owned.user = ""
					control.user = ""
				}
				if scenario == "invalid-user" {
					owned.user = "../Me"
					control.user = "../Me"
				}
				verified := false
				if mode == "primary" {
					result := owned.logoutPrimaryProof()
					verified = result.Verified
					if result.Logout != 204 {
						t.Error("owned logout not attempted")
					}
				} else {
					result := owned.logoutWithProtectedDenial(control)
					verified = result.Verified
					if result.LogoutStatus != 204 {
						t.Error("owned logout not attempted")
					}
				}
				if verified != (scenario == "success") || logouts != 1 {
					t.Fatalf("wrong proof verified=%t logouts=%d", verified, logouts)
				}
				if (scenario == "missing-user" || scenario == "invalid-user") && protected != 0 {
					t.Error("missing identity sent protected request")
				}
				if mode == "primary" && anonymous != 1 {
					t.Error("missing service health")
				}
			})
		}
	}
}

func labProbeUserIdentity(ref labReferenceData, loginUser, sessionUser string) (string, error) {
	if ref.protocol != "emby" {
		return "", nil
	}
	if !labValidUserID(ref.user) || !labValidUserID(loginUser) || !strings.EqualFold(loginUser, ref.user) || (sessionUser != "" && !strings.EqualFold(sessionUser, ref.user)) {
		return "", fmt.Errorf("lab login user mismatch or missing; credential quarantined")
	}
	return loginUser, nil
}

func TestLabEmbyLoginUserBinding(t *testing.T) {
	const own = "0123456789abcdef0123456789abcdef"
	const foreign = "fedcba9876543210fedcba9876543210"
	for _, tc := range []struct {
		name, primary, login, session string
		valid                         bool
	}{
		{"same", own, own, own, true}, {"session-optional", own, own, "", true},
		{"missing-primary", "", own, "", false}, {"missing-login", own, "", "", false},
		{"foreign-login", own, foreign, "", false}, {"foreign-session", own, own, foreign, false},
		{"invalid-path", own, "../Me", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := labProbeUserIdentity(labReferenceData{protocol: "emby", user: tc.primary}, tc.login, tc.session)
			if (err == nil) != tc.valid || (tc.valid && id != tc.login) || (!tc.valid && id != "") {
				t.Error("incorrect login identity binding")
			}
		})
	}
}

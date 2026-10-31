package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/store"
)

type offerAdmissionIdentity struct{ frontendtest.Identity }

func (offerAdmissionIdentity) ResolveDevice(context.Context, string, frontend.Device) (*core.Viewer, time.Time, error) {
	return &core.Viewer{ID: 1}, time.Now().Add(time.Hour), nil
}

// Hooks stop the actual HTTP admission at completed broker boundaries. The
// other goroutine commits revocation/expiry before allowing admission onward.
type offerAdmissionBroker struct {
	*Broker
	boundary string
	reached  chan struct{}
	resume   chan struct{}
	streams  atomic.Int32
}

func (b *offerAdmissionBroker) pause(at string) {
	if b.boundary == at {
		close(b.reached)
		<-b.resume
	}
}
func (b *offerAdmissionBroker) Plan(ctx context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
	b.pause("before-plan")
	return b.Broker.Plan(ctx, req)
}
func (b *offerAdmissionBroker) Open(ctx context.Context, req frontend.OpenRequest) (frontend.Session, error) {
	session, err := b.Broker.Open(ctx, req)
	if err == nil {
		b.pause("before-match")
	}
	return session, err
}
func (b *offerAdmissionBroker) StartStream(ctx context.Context, access frontend.SessionAccess, id string, req frontend.StreamRequest) (string, error) {
	path, err := b.Broker.StartStream(ctx, access, id, req)
	if err == nil {
		b.streams.Add(1)
		b.pause("before-publication")
	}
	return path, err
}

func TestSourceOfferHTTPAdmissionExpiryAndRevocationBoundaries(t *testing.T) {
	for _, kind := range []string{"detail", "version"} {
		for _, change := range []string{"expiry", "revocation", "offer-revocation", "source-replacement", "request-cancel"} {
			for _, boundary := range []string{"before-plan", "before-match", "before-publication", "after-publication"} {
				if (change == "source-replacement" && (kind != "detail" || boundary == "after-publication")) || (change == "request-cancel" && boundary == "after-publication") {
					continue
				}
				t.Run(kind+"/"+change+"/"+boundary, func(t *testing.T) {
					ctx := context.Background()
					admissionCtx, cancelAdmission := context.WithCancel(ctx)
					defer cancelAdmission()
					db := store.OpenTest(t)
					path := filepath.Join(t.TempDir(), "source.mp4")
					if err := os.WriteFile(path, []byte("immutable source"), 0600); err != nil {
						t.Fatal(err)
					}
					file, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					playback := &offerRevisionPlayback{&localRevisionPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}, source: localRevisionFile{file}}}
					var denied atomic.Bool
					active := func(_ context.Context, id int64) (*core.Viewer, error) {
						if denied.Load() {
							return nil, core.ErrUnauthorized
						}
						return &core.Viewer{ID: id}, nil
					}
					events := NewEventBus(nil, active)
					defer events.Close()
					lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
						return &core.Title{Key: key, Type: core.TypeMovie}, nil
					}
					broker, err := NewBroker(BrokerDeps{DB: db, Events: events, Playback: playback, Lookup: lookup, MediaLookup: lookup, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Progress: &fakeProgress{}, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Variants: func(context.Context, *core.Viewer, *core.Title, core.ClientCaps) ([]core.ReleaseVariant, error) {
						return []core.ReleaseVariant{{ID: "local:test"}}, nil
					}})
					if err != nil {
						t.Fatal(err)
					}
					defer broker.Close(ctx)
					scope := frontend.VersionScope{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "tv"}, Protocol: "silo"}
					item := core.MovieKey(1)
					versionAPI, err := NewVersionHandles(db, broker, "silo")
					if err != nil {
						t.Fatal(err)
					}
					target := frontend.SourceOfferTarget{Item: item}
					if kind == "detail" {
						detail, err := broker.DetailPlan(ctx, frontend.PlanRequest{Viewer: scope.Viewer, Device: scope.Device, Protocol: "silo", Item: item, Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, AudioStream: -1, SubtitleStream: -1}}})
						if err != nil {
							t.Fatal(err)
						}
						target.DetailSource, target.ETag = detail.DetailSource, detail.ETag
					} else {
						target.VersionHandle, err = versionAPI.Issue(ctx, scope, item, "local:test")
						if err != nil {
							t.Fatal(err)
						}
					}
					offers, err := NewSourceOffers(db, broker, "silo")
					if err != nil {
						t.Fatal(err)
					}
					offer, err := offers.Issue(ctx, scope, target)
					if err != nil {
						t.Fatal(err)
					}
					wrapped := &offerAdmissionBroker{Broker: broker, boundary: boundary, reached: make(chan struct{}), resume: make(chan struct{})}
					inst, err := (silo.Frontend{MachineID: "admission-test"}).Mount(ctx, frontend.Host{Identity: offerAdmissionIdentity{frontendtest.Identity{ResolveFunc: func(context.Context, string) (*core.Viewer, error) { return scope.Viewer, nil }}}, Sessions: wrapped, Capabilities: frontendtest.Capabilities{}, VersionHandles: versionAPI, SourceOffers: offers})
					if err != nil {
						t.Fatal(err)
					}
					request := func(method, path, body string) *httptest.ResponseRecorder {
						r := httptest.NewRequest(method, path, strings.NewReader(body))
						if method == "POST" {
							r = r.WithContext(admissionCtx)
						}
						r.Header.Set("Authorization", "Bearer grant")
						r.Header.Set("X-Silo-Device-Id", "tv")
						w := httptest.NewRecorder()
						inst.Handler().ServeHTTP(w, r)
						return w
					}
					caps := request("GET", "/api/v2/playback/capabilities", "")
					var discovery struct {
						Installation string `json:"installation_id"`
					}
					if caps.Code != 200 || json.Unmarshal(caps.Body.Bytes(), &discovery) != nil || discovery.Installation == "" {
						t.Fatalf("caps %d %s", caps.Code, caps.Body.String())
					}
					raw, _ := json.Marshal(map[string]any{"protocol_version": 3, "file_id": offer.ID, "installation_id": discovery.Installation, "playback_attempt_id": "attempt"})
					result := make(chan *httptest.ResponseRecorder, 1)
					go func() { result <- request("POST", "/api/v2/playback/start", string(raw)) }()
					resumed := false
					defer func() {
						if !resumed {
							close(wrapped.resume)
						}
						select {
						case <-result:
						case <-time.After(5 * time.Second):
						}
					}()
					var response *httptest.ResponseRecorder
					select {
					case <-wrapped.reached:
					case response = <-result:
						if boundary != "after-publication" {
							t.Fatalf("admission never reached boundary %d %s", response.Code, response.Body.String())
						}
					case <-time.After(5 * time.Second):
						t.Fatal("admission boundary timeout")
					}
					if change == "source-replacement" {
						if err = os.Rename(path, path+".old"); err == nil {
							err = os.WriteFile(path, []byte("different source"), 0600)
						}
					} else if change == "request-cancel" {
						cancelAdmission()
					} else if change == "offer-revocation" {
						if err = offers.Revoke(ctx, scope, offer.ID); err != nil {
							t.Fatal(err)
						}
					} else if kind == "detail" {
						if change == "expiry" {
							_, err = db.Exec(`UPDATE sessions_detail_sources SET expires=1 WHERE handle=?`, target.DetailSource)
						} else {
							_, err = db.Exec(`DELETE FROM sessions_detail_sources WHERE handle=?`, target.DetailSource)
						}
					} else {
						digest := sha256.Sum256([]byte(target.VersionHandle))
						value := hex.EncodeToString(digest[:])
						if change == "expiry" {
							_, err = db.Exec(`UPDATE capability_headers SET expires=1 WHERE namespace='sessions.version' AND digest=?`, value)
						} else {
							_, err = db.Exec(`DELETE FROM capability_headers WHERE namespace='sessions.version' AND digest=?`, value)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					close(wrapped.resume)
					resumed = true
					if response == nil {
						select {
						case response = <-result:
						case <-time.After(5 * time.Second):
							t.Fatal("admission did not finish")
						}
					}
					// Put the consumed result back for the deferred completion check.
					result <- response
					if boundary == "after-publication" {
						if response.Code != http.StatusCreated {
							t.Fatalf("healthy admission failed: %d %s", response.Code, response.Body.String())
						}
						var payload struct {
							Plan struct {
								Stream struct {
									URL string `json:"url"`
								} `json:"stream"`
							} `json:"playback_plan"`
						}
						if json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Plan.Stream.URL == "" {
							t.Fatal("no admitted media URL")
						}
						media := request("GET", payload.Plan.Stream.URL, "")
						if media.Code != http.StatusOK || media.Body.String() != "source bytes" {
							t.Fatalf("selector lifetime revoked independent matching session: %d %s", media.Code, media.Body.String())
						}
						denied.Store(true)
						media = request("GET", payload.Plan.Stream.URL, "")
						if media.Code == http.StatusOK || strings.Contains(media.Body.String(), "source bytes") {
							t.Fatalf("revoked viewer accessed admitted media: %d %s", media.Code, media.Body.String())
						}
						return
					}
					if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "/api/v2/stream/") {
						t.Fatalf("revoked/expired selector published media: %d %s", response.Code, response.Body.String())
					}
					if boundary != "before-publication" && wrapped.streams.Load() != 0 {
						t.Fatal("stream started after committed selector retirement")
					}
					if sessions, err := broker.Sessions(ctx, frontend.SessionAccess{Viewer: scope.Viewer, Device: scope.Device, Protocol: "silo"}); err != nil || len(sessions) != 0 {
						t.Fatalf("failed admission left active sessions: %+v %v", sessions, err)
					}
				})
			}
		}
	}
}

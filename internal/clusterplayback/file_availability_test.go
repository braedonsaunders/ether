package clusterplayback

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/playback"
)

func TestRemoteFileCheckUsesSourceWithoutPlaybackAdmission(t *testing.T) {
	manager, err := playback.New(playback.Deps{Catalog: fixtureCatalog{}, Resolver: fixtureResolver{}, Engine: fixtureEngine{}, Transcoder: &fixtureEncoder{}, SelectProfile: func(core.ClientCaps, *core.User) core.QualityProfile { return core.QualityProfile{} }}, config.Defaults(t.TempDir()), playback.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	sourceID := nodewire.Identity{Cluster: "offline", NodeID: "source", Boot: "source-boot"}
	frontID := nodewire.Identity{Cluster: "offline", NodeID: "front", Boot: "front-boot"}
	guards := fixtureGuards(t, sourceID, frontID)
	var denied atomic.Bool
	source := &Runtime{o: Options{Self: sourceID, Guard: guards[0], Playback: manager, Eligible: func(_ context.Context, id nodewire.Identity, role config.NodeRole) error {
		if id == sourceID && role == config.RoleSource || id == frontID && role == config.RoleServer {
			return nil
		}
		return ErrDenied
	}, ActiveViewer: func(_ context.Context, id int64) (*core.Viewer, error) {
		if id == 9 {
			return &core.Viewer{ID: id}, nil
		}
		return nil, ErrDenied
	}, SourcePolicy: func(context.Context, *core.Viewer, *core.PlaySession) error {
		if denied.Load() {
			return ErrDenied
		}
		return nil
	}}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(source.sourceHTTP))
	server.TLS = guards[0].ServerTLS()
	server.StartTLS()
	defer server.Close()
	front := &Runtime{o: Options{Self: frontID, Guard: guards[1], Endpoint: func(context.Context, nodewire.Identity) (string, error) { return server.URL, nil }}}
	remote := &RemotePlayback{r: front, o: RemotePlaybackOptions{SelectSource: func(context.Context) (nodewire.Identity, error) { return sourceID, nil }}}
	live, err := remote.CheckFileAvailability(context.Background(), &core.User{ID: 9}, core.MovieKey(1), core.ClientCaps{ClientID: "file-probe", Protocol: "plex"}, "")
	if err != nil || !live {
		t.Fatalf("live=%v err=%v", live, err)
	}
	if len(manager.Sessions()) != 0 || len(source.preparations) != 0 {
		t.Fatal("file check created a playback preparation")
	}
	denied.Store(true)
	if _, err = remote.CheckFileAvailability(context.Background(), &core.User{ID: 9}, core.MovieKey(1), core.ClientCaps{}, ""); err == nil {
		t.Fatal("source policy bypass")
	}
	if _, err = remote.CheckFileAvailability(context.Background(), &core.User{ID: 8}, core.MovieKey(1), core.ClientCaps{}, ""); err == nil {
		t.Fatal("viewer policy bypass")
	}
}

package frontend

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type testFrontend struct {
	id       Protocol
	instance *testInstance
	err      error
	hostID   *Protocol
}

func (f *testFrontend) Describe() Descriptor { return Descriptor{ID: f.id} }
func (f *testFrontend) Mount(_ context.Context, h Host) (Instance, error) {
	if f.hostID != nil {
		*f.hostID = Protocol(h.Config()["id"].(string))
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.instance, nil
}

type testInstance struct {
	closed   int
	closeErr error
}

func (i *testInstance) Handler() http.Handler         { return http.NotFoundHandler() }
func (i *testInstance) Listeners() []ListenerSpec     { return nil }
func (i *testInstance) Discovery() []Announcer        { return nil }
func (i *testInstance) Status(context.Context) Status { return Status{} }
func (i *testInstance) Close(context.Context) error   { i.closed++; return i.closeErr }
func TestRegistryLifecycle(t *testing.T) {
	ctx := context.Background()
	var bound Protocol
	r := NewRegistry(func(id Protocol) Host { return Host{Config: func() Settings { return Settings{"id": string(id)} }} })
	instance := &testInstance{}
	f := &testFrontend{id: "plex", instance: instance, hostID: &bound}
	if err := r.Register(f); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(f); err == nil {
		t.Fatal("duplicate registration")
	}
	if _, err := r.Mount(ctx, "missing"); !errors.Is(err, ErrNotRegistered) {
		t.Fatal(err)
	}
	got, err := r.Mount(ctx, "plex")
	if err != nil || got != instance || bound != "plex" {
		t.Fatal(got, err, bound)
	}
	if _, err := r.Mount(ctx, "plex"); !errors.Is(err, ErrAlreadyMounted) {
		t.Fatal(err)
	}
	instance.closeErr = errors.New("busy")
	if err := r.Unmount(ctx, "plex"); err == nil {
		t.Fatal("close failure lost")
	}
	if _, ok := r.Instance("plex"); !ok {
		t.Fatal("lost instance on close failure")
	}
	instance.closeErr = nil
	if err := r.Unmount(ctx, "plex"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Instance("plex"); ok {
		t.Fatal("still mounted")
	}
	if _, err := r.Mount(ctx, "plex"); err != nil {
		t.Fatal(err)
	}
	if instance.closed != 2 {
		t.Fatal(instance.closed)
	}
}
func TestFailedMountCanRetry(t *testing.T) {
	r := NewRegistry(func(Protocol) Host { return Host{} })
	f := &testFrontend{id: "test", err: errors.New("failed"), instance: &testInstance{}}
	if err := r.Register(f); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Mount(context.Background(), "test"); err == nil {
		t.Fatal("expected failure")
	}
	f.err = nil
	if _, err := r.Mount(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
}

type resolverFunc func(context.Context, string, string) (*core.Viewer, error)

func (f resolverFunc) Resolve(ctx context.Context, surface, credential string) (*core.Viewer, error) {
	return f(ctx, surface, credential)
}
func TestBoundIdentity(t *testing.T) {
	var gotSurface, gotCredential string
	resolver := resolverFunc(func(_ context.Context, surface, credential string) (*core.Viewer, error) {
		gotSurface, gotCredential = surface, credential
		return &core.Viewer{}, nil
	})
	identity := BindIdentity(resolver, "jellyfin")
	if _, err := identity.Resolve(context.Background(), "secret"); err != nil || gotSurface != "jellyfin" || gotCredential != "secret" {
		t.Fatal(gotSurface, gotCredential, err)
	}
	if _, err := BindIdentity(nil, "jellyfin").Resolve(context.Background(), "secret"); err == nil {
		t.Fatal("nil resolver accepted")
	}
}

package frontend

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

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

type reentrantFrontend struct {
	registry *Registry
	gate     chan struct{}
	entered  chan struct{}
}

func (f *reentrantFrontend) Describe() Descriptor {
	if f.registry != nil {
		_ = f.registry.Descriptors()
	}
	return Descriptor{ID: "reentrant"}
}
func (f *reentrantFrontend) ValidateSettings(Settings) error {
	_ = f.registry.Descriptors()
	return nil
}
func (f *reentrantFrontend) Mount(ctx context.Context, host Host) (Instance, error) {
	_ = host.Config()
	_ = f.registry.Descriptors()
	if f.entered != nil {
		close(f.entered)
		<-f.gate
	}
	return &reentrantInstance{registry: f.registry, config: host.Config}, nil
}

type reentrantInstance struct {
	registry *Registry
	config   func() Settings
}

func (*reentrantInstance) Handler() http.Handler     { return http.NotFoundHandler() }
func (*reentrantInstance) Listeners() []ListenerSpec { return nil }
func (*reentrantInstance) Discovery() []Announcer    { return nil }
func (i *reentrantInstance) Status(context.Context) Status {
	_ = i.registry.Descriptors()
	_, _ = i.registry.Instance("reentrant")
	return Status{}
}
func (i *reentrantInstance) Close(ctx context.Context) error {
	_ = i.config()
	_ = i.registry.Descriptors()
	i.Status(ctx)
	return nil
}
func TestRegistryCallbacksCanReenter(t *testing.T) {
	var r *Registry
	r = NewRegistry(func(id Protocol) Host {
		return Host{Config: func() Settings { _, _ = r.ValidateSettings(id, nil); return Settings{} }}
	})
	f := &reentrantFrontend{registry: r}
	if err := r.Register(f); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Mount(context.Background(), "reentrant")
		if err == nil {
			err = r.Unmount(context.Background(), "reentrant")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("registry callback deadlocked")
	}
}
func TestRegistrySerializesConcurrentMountAndUnmount(t *testing.T) {
	r := NewRegistry(func(Protocol) Host { return Host{Config: func() Settings { return nil }} })
	f := &reentrantFrontend{registry: r, entered: make(chan struct{}), gate: make(chan struct{})}
	if err := r.Register(f); err != nil {
		t.Fatal(err)
	}
	mounted := make(chan error, 1)
	unmounted := make(chan error, 1)
	go func() { _, err := r.Mount(context.Background(), "reentrant"); mounted <- err }()
	select {
	case <-f.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("mount did not enter")
	}
	go func() { unmounted <- r.Unmount(context.Background(), "reentrant") }()
	select {
	case err := <-unmounted:
		t.Fatalf("unmount interleaved with mount: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(f.gate)
	for _, ch := range []<-chan error{mounted, unmounted} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("transition did not finish")
		}
	}
	if _, ok := r.Instance("reentrant"); ok {
		t.Fatal("instance remained mounted")
	}
}

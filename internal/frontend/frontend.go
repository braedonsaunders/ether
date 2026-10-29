package frontend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type Descriptor struct {
	ID             Protocol `json:"id"`
	Name           string   `json:"name"`
	Logo           string   `json:"logo,omitempty"`
	Version        string   `json:"version,omitempty"`
	DocsURL        string   `json:"docsUrl,omitempty"`
	DefaultPort    int      `json:"defaultPort,omitempty"`
	Clients        []string `json:"clients,omitempty"`
	SettingsSchema any      `json:"settingsSchema,omitempty"`
}
type ListenerSpec struct {
	Address    string `json:"address"`
	PathPrefix string `json:"pathPrefix,omitempty"`
	TLS        bool   `json:"tls"`
}
type Status struct {
	Addresses     []string `json:"addresses"`
	LAN           string   `json:"lan"`
	WAN           string   `json:"wan"`
	Clients24h    int      `json:"clients24h"`
	LiveStreams   int      `json:"liveStreams"`
	TargetVersion string   `json:"targetVersion"`
	Conformance   string   `json:"conformance"`
	Error         string   `json:"error,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}
type Announcer interface {
	Start(context.Context) error
	Close() error
}
type Frontend interface {
	Describe() Descriptor
	Mount(context.Context, Host) (Instance, error)
}
type Instance interface {
	Handler() http.Handler
	Listeners() []ListenerSpec
	Discovery() []Announcer
	Status(context.Context) Status
	Close(context.Context) error
}

// SurfaceIdentity is bound to one protocol by the host factory.
type SurfaceIdentity interface {
	Resolve(context.Context, string) (*core.Viewer, error)
}

// BindIdentity prevents a frontend from choosing a different authentication surface.
func BindIdentity(resolver core.Identity, surface Protocol) SurfaceIdentity {
	return boundIdentity{resolver: resolver, surface: surface}
}

type boundIdentity struct {
	resolver core.Identity
	surface  Protocol
}

func (b boundIdentity) Resolve(ctx context.Context, credential string) (*core.Viewer, error) {
	if b.resolver == nil || b.surface == "" {
		return nil, errors.New("identity surface unavailable")
	}
	return b.resolver.Resolve(ctx, string(b.surface), credential)
}

type Settings map[string]any

// Subscriber delivers events across nodes until the context is cancelled.
type Subscriber interface {
	Subscribe(context.Context, EventScope) (<-chan Event, error)
}

// Images serves artwork through an explicit host-owned boundary.
type Images interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}
type Host struct {
	Experience Experience
	Sessions   Sessions
	Identity   SurfaceIdentity
	IDs        ids.Codecs
	Events     Subscriber
	Images     Images
	Config     func() Settings
	Log        *slog.Logger
}

var ErrNotRegistered = errors.New("frontend not registered")
var ErrAlreadyMounted = errors.New("frontend already mounted")

// Registry serializes lifecycle transitions so a protocol has at most one
// live instance. The host factory must bind identity to the requested surface.
type Registry struct {
	mu      sync.Mutex
	entries map[Protocol]*entry
	host    func(Protocol) Host
}
type entry struct {
	frontend Frontend
	instance Instance
}

func NewRegistry(host func(Protocol) Host) *Registry {
	return &Registry{entries: make(map[Protocol]*entry), host: host}
}
func (r *Registry) Register(f Frontend) error {
	if f == nil {
		return errors.New("nil frontend")
	}
	id := f.Describe().ID
	if id == "" {
		return errors.New("empty frontend id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[id]; ok {
		return fmt.Errorf("frontend %q already registered", id)
	}
	r.entries[id] = &entry{frontend: f}
	return nil
}
func (r *Registry) Mount(ctx context.Context, id Protocol) (Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return nil, ErrNotRegistered
	}
	if e.instance != nil {
		return nil, ErrAlreadyMounted
	}
	if r.host == nil {
		return nil, errors.New("missing host factory")
	}
	instance, err := e.frontend.Mount(ctx, r.host(id))
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, errors.New("frontend returned nil instance")
	}
	e.instance = instance
	return instance, nil
}
func (r *Registry) Unmount(ctx context.Context, id Protocol) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return ErrNotRegistered
	}
	if e.instance == nil {
		return nil
	}
	if err := e.instance.Close(ctx); err != nil {
		return err
	}
	e.instance = nil
	return nil
}
func (r *Registry) Instance(id Protocol) (Instance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok || e.instance == nil {
		return nil, false
	}
	return e.instance, true
}
func (r *Registry) Descriptors() []Descriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Descriptor, 0, len(r.entries))
	for _, e := range r.entries {
		result = append(result, e.frontend.Describe())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

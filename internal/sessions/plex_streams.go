package sessions

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const maxPlexStreamRefs = 200_000

// PlexStreamBinding is one Plex-visible stream id. WireID is zero on bind;
// BindPlexStream assigns it. SessionID is empty for library metadata and is
// the broker session id after Open.
type PlexStreamBinding struct {
	WireID    int64
	SessionID string
	Key       core.RatingKey
	Index     int
	Info      core.StreamInfo
}

// PlexStreamTable is the visible-id map. Plex offer ids are not stored here.
type PlexStreamTable struct {
	mu   sync.Mutex
	byID map[int64]plexStreamRef
}

type plexStreamRef struct {
	binding PlexStreamBinding
	slot    string
}

// NewPlexStreamTable is the visible-id map used by the broker and by Plex
// fixtures that have no broker mounted.
func NewPlexStreamTable() *PlexStreamTable {
	return &PlexStreamTable{byID: map[int64]plexStreamRef{}}
}

// PlexWireID is the positive int32-range id Plex clients already parse.
// Offer rows use it directly and are not stream bindings.
func PlexWireID(key core.RatingKey, slot string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.FormatInt(int64(key), 10) + "/" + slot))
	return int64(h.Sum64()%2_000_000_000) + 1
}

// PlexStreamSlot is the stable identity of a stream within a title.
func PlexStreamSlot(index int, info core.StreamInfo) string { return plexStreamSlot(index, info) }

func plexStreamSlot(index int, info core.StreamInfo) string {
	if info.External && info.ExternalURL != "" {
		return "external/" + info.ExternalURL
	}
	if info.Type == "audio" || info.Type == "subtitle" {
		raw, _ := json.Marshal([]any{index, info.Type, info.Language, info.Codec, info.Channels, info.Title, info.Forced, info.External, info.ExternalURL})
		return "track-v2/" + string(raw)
	}
	return strconv.Itoa(index)
}

// PlexStreamsMatch reports whether two renders are the same visible stream.
func PlexStreamsMatch(a, b core.StreamInfo) bool { return plexSameStream(a, b) }

func plexSameStream(a, b core.StreamInfo) bool {
	return a.Type == b.Type && a.Codec == b.Codec && a.Language == b.Language &&
		a.Channels == b.Channels && a.Title == b.Title && a.Forced == b.Forced &&
		a.External == b.External && a.ExternalURL == b.ExternalURL
}

// Bind assigns or reuses the wire id for this slot. It does not authorize.
func (t *PlexStreamTable) Bind(binding PlexStreamBinding) (int64, error) {
	if t == nil {
		return 0, core.ErrNotAvailable
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.byID) >= maxPlexStreamRefs {
		t.byID = map[int64]plexStreamRef{}
	}
	slot := plexStreamSlot(binding.Index, binding.Info)
	id := PlexWireID(binding.Key, slot)
	for {
		ref, ok := t.byID[id]
		if !ok || (ref.binding.Key == binding.Key && ref.slot == slot && plexSameStream(ref.binding.Info, binding.Info)) {
			if ok && binding.SessionID != "" {
				ref.binding.SessionID = binding.SessionID
				ref.binding.Index = binding.Index
				ref.binding.Info = binding.Info
				ref.binding.WireID = id
				t.byID[id] = ref
				return id, nil
			}
			stored := binding
			stored.WireID = id
			if ok && stored.SessionID == "" {
				stored.SessionID = ref.binding.SessionID
			}
			t.byID[id] = plexStreamRef{binding: stored, slot: slot}
			return id, nil
		}
		id++
	}
}

// Lookup reports the binding. False means the id was never bound.
func (t *PlexStreamTable) Lookup(id int64) (PlexStreamBinding, bool) {
	if t == nil || id == 0 {
		return PlexStreamBinding{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ref, ok := t.byID[id]
	return ref.binding, ok
}

type plexServe struct {
	remote bool
	format string
}

type plexServeKey struct{}

// WithPlexServe carries the request's remote bit and subtitle format into
// ResolvePlexStream and OpenPlexSidecar without changing their signatures.
func WithPlexServe(ctx context.Context, remote bool, format string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, plexServeKey{}, plexServe{remote: remote, format: format})
}

func plexServeFrom(ctx context.Context) plexServe {
	if ctx == nil {
		return plexServe{}
	}
	v, _ := ctx.Value(plexServeKey{}).(plexServe)
	return v
}

// BindPlexStream assigns the visible id. WireID on the binding must be zero.
// Library metadata calls this before a viewer is known, so it does not
// authorize. Resolve and Open do.
func (b *Broker) BindPlexStream(_ context.Context, access frontend.SessionAccess, binding PlexStreamBinding) (int64, error) {
	if b == nil || access.Protocol != "plex" || binding.WireID != 0 {
		return 0, core.ErrUnauthorized
	}
	return b.plexStreams.Bind(binding)
}

// ResolvePlexStream returns a bound stream. An unknown id is ErrNotFound.
// A viewer who may not play is ErrUnauthorized, not a miss.
func (b *Broker) ResolvePlexStream(ctx context.Context, access frontend.SessionAccess, wireID int64) (PlexStreamBinding, error) {
	if b == nil || access.Protocol != "plex" || wireID == 0 {
		return PlexStreamBinding{}, core.ErrUnauthorized
	}
	binding, ok := b.plexStreams.Lookup(wireID)
	if !ok {
		return PlexStreamBinding{}, core.ErrNotFound
	}
	viewer, err := b.confirmPlex(ctx, access)
	if err != nil {
		return PlexStreamBinding{}, err
	}
	// The wire id is shared by every viewer of the title. A session id
	// attached after Open is returned only to the viewer who owns it.
	if binding.SessionID != "" && !b.plexSessionOwned(binding.SessionID, viewer.ID) {
		binding.SessionID = ""
	}
	return binding, nil
}

// OpenPlexSidecar returns an external sidecar only after the acceptance,
// fenced policy, stream binding, and session agree. Failure returns no reader.
func (b *Broker) OpenPlexSidecar(ctx context.Context, access frontend.SessionAccess, receipt auth.Acceptance, wireID int64) (io.ReadCloser, core.StreamInfo, error) {
	if b == nil || access.Protocol != "plex" {
		return nil, core.StreamInfo{}, core.ErrUnauthorized
	}
	if b.deps.ReceiptCurrent != nil && (!receipt.Valid() || b.deps.ReceiptCurrent(ctx, receipt) != nil) {
		return nil, core.StreamInfo{}, core.ErrUnauthorized
	}
	binding, err := b.ResolvePlexStream(ctx, access, wireID)
	if err != nil {
		return nil, core.StreamInfo{}, err
	}
	viewer, err := b.confirmPlex(ctx, access)
	if err != nil {
		return nil, core.StreamInfo{}, err
	}
	if b.deps.Lookup != nil {
		if _, err := b.deps.Lookup(ctx, viewer, core.Device{}, binding.Key); err != nil {
			return nil, core.StreamInfo{}, core.ErrNotFound
		}
	}
	if !binding.Info.External || binding.Info.ExternalURL == "" || binding.Info.Type != "subtitle" {
		return nil, core.StreamInfo{}, core.ErrNotFound
	}
	reader, info, err := b.openExternalSidecar(ctx, binding.Info)
	if err != nil || reader == nil {
		if reader != nil {
			_ = reader.Close()
		}
		if err == nil {
			err = core.ErrNotFound
		}
		return nil, core.StreamInfo{}, err
	}
	return reader, info, nil
}

func (b *Broker) confirmPlex(ctx context.Context, access frontend.SessionAccess) (*core.Viewer, error) {
	return b.confirm(ctx, access.Viewer, core.ClientCaps{Remote: plexServeFrom(ctx).remote})
}

func (b *Broker) plexSessionOwned(id string, viewer int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sessions[id]
	return s != nil && s.public.Protocol == "plex" && s.public.ViewerID == viewer
}

func (b *Broker) openExternalSidecar(ctx context.Context, info core.StreamInfo) (io.ReadCloser, core.StreamInfo, error) {
	src := info.ExternalURL
	if strings.HasPrefix(src, "ether-subtitle:") {
		if b.deps.OpenSubtitle == nil {
			return nil, core.StreamInfo{}, core.ErrNotAvailable
		}
		format := plexServeFrom(ctx).format
		if format == "" {
			format = "srt"
		}
		reader, actual, err := b.deps.OpenSubtitle(src, format)
		if err != nil {
			return nil, core.StreamInfo{}, err
		}
		if actual != "" {
			info.Codec = actual
		}
		return reader, info, nil
	}
	u, err := url.Parse(src)
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, core.StreamInfo{}, err
		}
		resp, err := plexSidecarClient.Do(req)
		if err != nil {
			return nil, core.StreamInfo{}, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, core.StreamInfo{}, core.ErrNotFound
		}
		return resp.Body, info, nil
	}
	path := src
	if err == nil && u.Scheme == "file" {
		path = u.Path
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, core.StreamInfo{}, core.ErrNotFound
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		_ = f.Close()
		return nil, core.StreamInfo{}, core.ErrNotFound
	}
	return f, info, nil
}

var plexSidecarClient = egress.Client(egress.CapDirect, 20*time.Second)

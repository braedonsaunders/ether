package onair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/onair/encode"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"github.com/braedonsaunders/ether/internal/store"
	"os"
	"strings"
	"testing"
	"time"
)

func testService(t *testing.T, o Options) *Service {
	t.Helper()
	db, e := store.Open(context.Background(), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	o.DB = db
	o.Dir = t.TempDir()
	s, e := New(o)
	if e != nil {
		t.Fatal(e)
	}
	s.Start()
	t.Cleanup(s.Close)
	return s
}
func TestAssetAddressAndUsage(t *testing.T) {
	s := testService(t, Options{})
	ctx := context.Background()
	b := []byte("font fixture used directly at storage boundary")
	a, e := s.putAsset(ctx, "one", "image", "", "image/png", b)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(b)
	if a.Digest != hex.EncodeToString(sum[:]) {
		t.Fatal("wrong digest")
	}
	a2, e := s.putAsset(ctx, "two", "image", "", "image/png", b)
	if e != nil || a2.Digest != a.Digest {
		t.Fatal("not deduplicated", e)
	}
	l, e := s.Look(ctx, "nocturne")
	if e != nil {
		t.Fatal(e)
	}
	l.Brand.Logo = a.Digest
	if _, e = s.SaveLook(ctx, l); e != nil {
		t.Fatal(e)
	}
	if e = s.DeleteAsset(ctx, a.Digest); e != ErrUsed {
		t.Fatalf("deleted referenced asset: %v", e)
	}
	if e = s.DeleteLook(ctx, l.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS livetv_channels(id TEXT PRIMARY KEY,data TEXT NOT NULL)`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.ExecContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?)`, "channel", `{"look":{"momentAssets":{"ident":"`+a.Digest+`"}}}`); e != nil {
		t.Fatal(e)
	}
	if e = s.DeleteAsset(ctx, a.Digest); e != ErrUsed {
		t.Fatalf("deleted channel asset: %v", e)
	}
	if _, e = s.db.ExecContext(ctx, `DELETE FROM livetv_channels WHERE id=?`, "channel"); e != nil {
		t.Fatal(e)
	}
	if e = s.DeleteAsset(ctx, a.Digest); e != nil {
		t.Fatal(e)
	}
	as, e := s.Assets(ctx)
	if e != nil || len(as) != 0 {
		t.Fatal("asset retained", e)
	}
}
func TestHashEveryInput(t *testing.T) {
	inputs := []any{"renderer", map[string]any{"duration": 8}, map[string]any{"accent": "red"}, map[string]string{"title": "one"}, map[string]string{"logo": "abc"}, encode.Profile}
	hash := func(v []any) string { return encode.Hash(v[0].(string), v[1], v[2], v[3], v[4], v[5]) }
	base := hash(inputs)
	for i := range inputs {
		v := append([]any{}, inputs...)
		v[i] = "changed"
		if hash(v) == base {
			t.Fatalf("input %d missing", i)
		}
	}
	a := encode.Hash("v", nil, nil, map[string]int{"a": 1, "b": 2}, nil, nil)
	b := encode.Hash("v", nil, nil, map[string]int{"b": 2, "a": 1}, nil, nil)
	if a != b {
		t.Fatal("map order affected content address")
	}
}
func TestEnsureQueuesOnly(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 20)
	s := testService(t, Options{Render: func(ctx context.Context, d scene.Document, b raster.Bindings, out string) error {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
		}
		return os.WriteFile(out, []byte("mp4"), 0600)
	}})
	done := make(chan struct{})
	go func() {
		s.Ensure(context.Background(), core.ClipRequest{LookID: "nocturne", Moment: "ident", Bind: map[string]string{"channel.name": "unique"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Ensure blocked on renderer")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	close(gate)
	_, e := families.Get("signal", "screen.card")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.UploadAsset(context.Background(), "bad.ttf", "font", "", bytes.NewBufferString(strings.Repeat("x", 20))); e == nil {
		t.Fatal("font without licence accepted")
	}
}

type draftModel struct {
	replies []string
	calls   int
}

func (m *draftModel) Enabled() bool { return true }
func (m *draftModel) Chat(_ context.Context, _ []llm.Message, _ bool) (string, error) {
	v := m.replies[min(m.calls, len(m.replies)-1)]
	m.calls++
	return v, nil
}

// A model correction may change the draft, but never writes owner records.
func TestDraftValidationNeverSaves(t *testing.T) {
	model := &draftModel{}
	s := testService(t, Options{Chat: model})
	base, e := s.Look(context.Background(), "nocturne")
	if e != nil {
		t.Fatal(e)
	}
	draft := base
	draft.Name = "Generated draft"
	draft.Brand.Tagline = strings.Repeat("x", 41)
	bad, _ := json.Marshal(draft)
	draft.Brand.Tagline = "A quieter evening"
	good, _ := json.Marshal(draft)
	model.replies = []string{string(bad), string(good)}
	got, e := s.Generate(context.Background(), "Make the motion calmer", "", base)
	if e != nil {
		t.Fatal(e)
	}
	if model.calls != 2 || got.Brand.Tagline != draft.Brand.Tagline {
		t.Fatal("validation retry did not correct the draft")
	}
	saved, e := s.Look(context.Background(), base.ID)
	if e != nil || saved.Name != base.Name {
		t.Fatal("draft mutated the saved look", e)
	}
}

// Shared styles must remain reusable when a channel chooses its own moment
// video. Both content and length participate in the content-addressed cache.
func TestChannelMomentAssetIsolation(t *testing.T) {
	look := familyLook("clean", "Clean")
	look.ID = "shared"
	look.Moments["ident"] = Moment{Duration: 8, AssetDigest: "style-video"}
	s := &Service{looks: map[string]Look{"shared": look}}
	req := core.ClipRequest{LookID: "shared", Moment: "ident"}
	inherited, ok := s.plan(req)
	if !ok {
		t.Fatal("missing style plan")
	}
	req.AssetDigest = "channel-video"
	req.AssetDuration = 12 * time.Second
	custom, ok := s.plan(req)
	if !ok || custom.AssetDigest != "channel-video" || custom.Doc.Duration != 12 || custom.Hash == inherited.Hash {
		t.Fatal("channel asset missing from render plan")
	}
	if s.looks["shared"].Moments["ident"].AssetDigest != "style-video" {
		t.Fatal("channel changed shared style")
	}
	req.AssetDuration = 8 * time.Second
	short, _ := s.plan(req)
	if short.Hash == custom.Hash {
		t.Fatal("asset duration absent from cache identity")
	}
	req.LookID = ""
	if _, ok := s.plan(req); !ok {
		t.Fatal("asset-only channel needs no style")
	}
}

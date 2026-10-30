package playback

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// restartHarnesses builds two managers over one database and data dir:
// the second is the same server after a restart, with empty in-memory
// warm state but the first manager's persisted resume records.
func restartHarnesses(t *testing.T, rels func() []*fakeRelease, augment func(*atomic.Int64) func(context.Context, *core.Title, *core.User, *core.Resolution, core.MediaSource, *core.MediaInfo)) (h1, h2 *harness, db *sql.DB, augments *atomic.Int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir := t.TempDir()
	augments = &atomic.Int64{}
	shared := func(h *harness, o *Options) {
		h.db = db
		h.dataDir = dir
		o.DataDir = dir
	}
	h1 = newHarness(t, rels(), []string{"rel1"}, shared)
	h2 = newHarness(t, rels(), []string{"rel1"}, shared)
	if augment != nil {
		fn := augment(augments)
		h1.m.deps.AugmentMedia = fn
		h2.m.deps.AugmentMedia = fn
	}
	return h1, h2, db, augments
}

const sidecarURL = "https://subs.example.test/dl/abc?token=sekrit"

func sidecarAugment(count *atomic.Int64) func(context.Context, *core.Title, *core.User, *core.Resolution, core.MediaSource, *core.MediaInfo) {
	return func(_ context.Context, _ *core.Title, _ *core.User, _ *core.Resolution, _ core.MediaSource, info *core.MediaInfo) {
		count.Add(1)
		info.Streams = append(info.Streams, core.StreamInfo{
			Index: 99, Type: "subtitle", Codec: "srt", Language: "en",
			External: true, ExternalURL: sidecarURL,
		})
	}
}

// The first Play after a restart of an already-played title must reuse
// the resume record's persisted probe and keyframe index instead of
// resolving and probing again, while per-viewer sidecar enrichment
// (never persisted, URL-bearing) is produced afresh.
func TestPersistedProbeReusedAfterRestart(t *testing.T) {
	rels := func() []*fakeRelease {
		r := mkv("rel1", 20<<20, 100*time.Minute)
		r.Height, r.Codec, r.BitrateKbps = 720, "hevc", 8000
		return []*fakeRelease{r}
	}
	h1, h2, db, augments := restartHarnesses(t, rels, sidecarAugment)
	ctx := context.Background()

	ps1 := h1.prepare(owner, movieKey, "client1")
	if got := h1.tr.probes; got != 1 {
		t.Fatalf("first Prepare probes = %d, want 1", got)
	}
	if ps1.Info.Estimated || ps1.Info.Duration != 100*time.Minute {
		t.Fatalf("first Prepare info = %+v", ps1.Info)
	}
	// The background index fill persists keyframes after playback starts;
	// seed the row it would write.
	points := []core.KeyframePoint{{Offset: 0, Time: 0}, {Offset: 1 << 20, Time: 5 * time.Minute}}
	if err := h1.m.resume.saveIndex(ctx, owner.ID, movieKey, "rel1", points); err != nil {
		t.Fatal(err)
	}
	// The credential-bearing sidecar URL must never reach disk.
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT info_json FROM playback_probe WHERE release_id='rel1'`).Scan(&raw); err != nil {
		t.Fatalf("persisted probe row: %v", err)
	}
	if strings.Contains(raw, sidecarURL) || strings.Contains(raw, "sekrit") || strings.Contains(raw, "externalUrl") {
		t.Fatalf("persisted probe leaks external subtitle URL: %s", raw)
	}
	if err := h1.m.Close(); err != nil {
		t.Fatal(err)
	}

	before := augments.Load()
	ps2 := h2.prepare(owner, movieKey, "client1")
	if got := h2.resolver.callCount(); got != 0 {
		t.Fatalf("post-restart Resolve calls = %d, want 0 (resume record)", got)
	}
	if got := h2.tr.probes; got != 0 {
		t.Fatalf("post-restart probes = %d, want 0 (persisted probe)", got)
	}
	if ps2.Info.Estimated {
		t.Fatalf("post-restart info fell back to estimated: %+v", ps2.Info)
	}
	if ps2.Info.Duration != 100*time.Minute || ps2.Info.Container != "mkv" {
		t.Fatalf("post-restart info = %+v", ps2.Info)
	}
	if v := ps2.Info.Video(); v == nil || v.Height != 720 || v.Codec != "hevc" {
		t.Fatalf("post-restart video stream = %+v", v)
	}
	if len(ps2.Info.Keyframes) != len(points) || ps2.Info.Keyframes[1] != points[1] {
		t.Fatalf("post-restart keyframes = %+v, want %+v", ps2.Info.Keyframes, points)
	}
	// Enrichment ran again and its sidecar is live for this session only.
	if augments.Load() != before+1 {
		t.Fatalf("AugmentMedia calls = %d, want one more after restart", augments.Load())
	}
	var ext *core.StreamInfo
	for i := range ps2.Info.Streams {
		if ps2.Info.Streams[i].External {
			ext = &ps2.Info.Streams[i]
		}
	}
	if ext == nil || ext.ExternalURL != sidecarURL {
		t.Fatalf("post-restart sidecar stream missing: %+v", ps2.Info.Streams)
	}
}

// A persisted probe describes one exact file. When the opened source no
// longer matches its recorded size, playback must probe again rather
// than trust stale facts.
func TestPersistedProbeRejectedOnSizeMismatch(t *testing.T) {
	h1, h2, _, _ := restartHarnesses(t, func() []*fakeRelease {
		return []*fakeRelease{mkv("rel1", 20<<20, 100*time.Minute)}
	}, nil)
	h1.prepare(owner, movieKey, "client1")
	if err := h1.m.Close(); err != nil {
		t.Fatal(err)
	}
	// The same release ID now opens a different-sized file.
	h2.engine.mu.Lock()
	h2.engine.releases["rel1"].Size = 30 << 20
	h2.engine.mu.Unlock()

	ps := h2.prepare(owner, movieKey, "client1")
	if got := h2.tr.probes; got != 1 {
		t.Fatalf("size-mismatch probes = %d, want 1 (stale probe refused)", got)
	}
	if ps.Info.Size != 30<<20 {
		t.Fatalf("size-mismatch info size = %d, want %d", ps.Info.Size, 30<<20)
	}
}

// Facts probed from one NZB payload must not serve a Prepare whose
// resume payload has since been replaced under the same release ID
// (same size included): the digest mismatch forces a fresh probe.
func TestPersistedProbeRejectedOnPayloadChange(t *testing.T) {
	h1, h2, db, _ := restartHarnesses(t, func() []*fakeRelease {
		return []*fakeRelease{mkv("rel1", 20<<20, 100*time.Minute)}
	}, nil)
	h1.prepare(owner, movieKey, "client1")
	if err := h1.m.Close(); err != nil {
		t.Fatal(err)
	}
	// The stored facts were probed from some other payload than the one
	// the resume record now carries.
	if _, err := db.ExecContext(context.Background(),
		`UPDATE playback_probe SET nzb_hash=? WHERE release_id='rel1'`, nzbHash([]byte("some other payload"))); err != nil {
		t.Fatal(err)
	}

	h2.prepare(owner, movieKey, "client1")
	if got := h2.resolver.callCount(); got != 0 {
		t.Fatalf("Resolve calls = %d, want 0 (resume record still applies)", got)
	}
	if got := h2.tr.probes; got != 1 {
		t.Fatalf("changed-payload probes = %d, want 1 (digest mismatch refused)", got)
	}
}

// Sanitization and round-trip of the persisted facts themselves.
func TestPersistedProbeFactsSanitizedAndBounded(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs, err := newResumeStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.close(context.Background()) })
	ctx := context.Background()
	info := core.MediaInfo{
		Container: "mkv", Duration: time.Hour, Size: 1234, BitrateKbps: 5000,
		Keyframes: []core.KeyframePoint{{Offset: 10, Time: time.Second}},
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Height: 1080},
			{Index: 1, Type: "subtitle", Codec: "srt", External: true, ExternalURL: sidecarURL},
			{Index: 2, Type: "audio", Codec: "aac", ExternalURL: sidecarURL},
		},
	}
	nzb := []byte("<nzb>payload-a</nzb>")
	if err := rs.saveProbe(ctx, 7, movieKey, "rel", nzb, info, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	// Same release ID and same size but a different NZB payload is not
	// the media these facts were probed from.
	if got, err := rs.loadProbe(ctx, 7, movieKey, "rel", []byte("<nzb>payload-b</nzb>")); err != nil || got != nil {
		t.Fatalf("changed-payload probe = %+v, %v, want nil miss", got, err)
	}
	got, err := rs.loadProbe(ctx, 7, movieKey, "rel", nzb)
	if err != nil || got == nil {
		t.Fatalf("loadProbe = %+v, %v", got, err)
	}
	if !got.needsAugment {
		t.Fatal("rebuilt probe must request AugmentMedia")
	}
	if len(got.info.Streams) != 2 {
		t.Fatalf("external stream not dropped: %+v", got.info.Streams)
	}
	for _, s := range got.info.Streams {
		if s.ExternalURL != "" {
			t.Fatalf("ExternalURL persisted on stream %+v", s)
		}
	}

	// Estimated (name-derived) info is never persisted as a probe.
	est := info
	est.Estimated = true
	if err := rs.saveProbe(ctx, 7, movie2, "rel", nzb, est, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if got, err := rs.loadProbe(ctx, 7, movie2, "rel", nzb); err != nil || got != nil {
		t.Fatalf("estimated probe persisted: %+v, %v", got, err)
	}

	// A corrupt row is a cache miss, not an error or a trusted probe.
	if _, err := db.ExecContext(ctx, `UPDATE playback_probe SET info_json='{nope'`); err != nil {
		t.Fatal(err)
	}
	if got, err := rs.loadProbe(ctx, 7, movieKey, "rel", nzb); err != nil || got != nil {
		t.Fatalf("corrupt probe = %+v, %v, want nil miss", got, err)
	}
}

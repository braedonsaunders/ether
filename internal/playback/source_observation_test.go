package playback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type historyFileSource struct {
	core.MediaSource
	file        *os.File
	inspections atomic.Int32
	deny        atomic.Bool
	unsupported bool
}

func (s *historyFileSource) LocalFile() *os.File {
	if s.deny.Load() {
		panic("report inspected local filesystem")
	}
	s.inspections.Add(1)
	if s.unsupported {
		return nil
	}
	return s.file
}
func (s *historyFileSource) ReadAt(b []byte, off int64) (int, error) { return s.file.ReadAt(b, off) }
func (s *historyFileSource) Close() error                            { return errors.Join(s.MediaSource.Close(), s.file.Close()) }

type historyFileEngine struct {
	core.Engine
	file        *os.File
	source      *historyFileSource
	unsupported bool
}

func (e *historyFileEngine) Open(ctx context.Context, id string, nzb []byte) (core.MediaSource, error) {
	src, err := e.Engine.Open(ctx, id, nzb)
	if err != nil {
		return nil, err
	}
	e.source = &historyFileSource{MediaSource: src, file: e.file, unsupported: e.unsupported}
	return e.source, nil
}
func historyFileFixture(t *testing.T) (*harness, *historyFileEngine, string) {
	t.Helper()
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	path := filepath.Join(t.TempDir(), "A.mkv")
	if err := os.WriteFile(path, expected("A", 0, 10000), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.m.Close(); f.Close() })
	e := &historyFileEngine{Engine: h.engine, file: f}
	h.m.deps.Engine = e
	return h, e, path
}
func TestHistoricalLocalObservationNeedsNoFilesystemOnReport(t *testing.T) {
	h, e, path := historyFileFixture(t)
	h.engine.mu.Lock()
	h.engine.releases["A"].HDR = true
	h.engine.mu.Unlock()
	ps := h.prepare(owner, movieKey, "historical-local")
	first, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || first.Revision == "" || first.ReleaseID != "A" || first.VideoCodec == "" || first.Height == 0 || first.HDR == nil {
		t.Fatalf("local observation=%+v %v", first, err)
	}
	inspections := e.source.inspections.Load()
	if inspections < 2 {
		t.Fatalf("open/probe validations=%d", inspections)
	}
	e.source.deny.Store(true)
	for i := 0; i < 20; i++ {
		got, err := h.m.CurrentSource(context.Background(), ps.ID)
		if err != nil || got.Revision != first.Revision {
			t.Fatalf("cached observation=%+v %v", got, err)
		}
	}
	if e.source.inspections.Load() != inspections {
		t.Fatal("report re-inspected file")
	}
	e.source.deny.Store(false)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || got.Revision != first.Revision {
		t.Fatalf("historical source changed without a new generation: %+v %v", got, err)
	}
	if err := h.m.PinSourceRevision(context.Background(), ps.ID, first.Revision); !errors.Is(err, core.ErrSourceChanged) {
		t.Fatalf("fresh pin accepted removed path: %v", err)
	}
	if first.HDR != nil {
		*first.HDR = !*first.HDR
		copy, err := h.m.CurrentSource(context.Background(), ps.ID)
		if err != nil || copy.HDR == nil || *copy.HDR == *first.HDR {
			t.Fatal("caller mutated cached HDR")
		}
	}
}
func TestHistoricalLocalObservationReplacementDuringProbeIsUnknown(t *testing.T) {
	h, _, path := historyFileFixture(t)
	var replacementErr error
	h.m.deps.AugmentMedia = func(_ context.Context, _ *core.Title, _ *core.User, _ *core.Resolution, _ core.MediaSource, _ *core.MediaInfo) {
		if err := os.Rename(path, path+".original"); err != nil {
			replacementErr = err
			return
		}
		if err := os.WriteFile(path, expected("A", 0, 10000), 0600); err != nil {
			replacementErr = err
			return
		}
	}
	ps := h.prepare(owner, movieKey, "replace-during-probe")
	if replacementErr != nil {
		t.Fatal(replacementErr)
	}
	got, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || got.Revision != "" || got.ReleaseID != "" || got.VideoCodec != "" || got.Height != 0 || got.HDR != nil {
		t.Fatalf("replacement exposed unverified facts: %+v %v", got, err)
	}
}
func TestHistoricalLocalObservationUnsupportedIdentityIsUnknown(t *testing.T) {
	h, e, _ := historyFileFixture(t)
	e.unsupported = true
	ps := h.prepare(owner, movieKey, "unsupported-local")
	e.source.deny.Store(true)
	got, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || got.Revision != "" || got.ReleaseID != "" || got.VideoCodec != "" || got.Height != 0 || got.HDR != nil {
		t.Fatalf("unsupported identity fabricated facts: %+v %v", got, err)
	}
	e.source.deny.Store(false)
}

func TestSourceSnapshotSurvivesFailoverAndStop(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "history-snapshot")
	first, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || first.Revision == "" || first.ReleaseID != "A" {
		t.Fatalf("first source: %+v %v", first, err)
	}
	if _, err = h.m.Failover(context.Background(), ps.ID, 0); err != nil {
		t.Fatal(err)
	}
	second, err := h.m.CurrentSource(context.Background(), ps.ID)
	if err != nil || second.ReleaseID != "B" || first.Revision == second.Revision || first.ReleaseID != "A" {
		t.Fatalf("source snapshots: %+v %+v %v", first, second, err)
	}
	h.m.Stop(ps.ID)
	if _, err = h.m.CurrentSource(context.Background(), ps.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("retired source: %v", err)
	}
}

func TestObservedHDRRequiresConsistentEvidence(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name  string
		video core.StreamInfo
		want  *bool
	}{
		{"unknown transfer", core.StreamInfo{}, nil},
		{"bt709 with unknown metadata", core.StreamInfo{ColorTransfer: "bt709"}, nil},
		{"bt709 with HDR10+", core.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &yes}, &yes},
		{"bt709 with DV configuration", core.StreamInfo{ColorTransfer: "bt709", DolbyVisionConfigurationPresent: &yes}, &yes},
		{"bt709 with DV enhancement", core.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &no, DolbyVisionConfigurationPresent: &no, DolbyVisionRPUPresent: &no, DolbyVisionELPresent: &yes}, &yes},
		{"PQ", core.StreamInfo{ColorTransfer: "smpte2084"}, &yes},
		{"explicit absence", core.StreamInfo{ColorTransfer: "bt709", HDR10Plus: &no, DolbyVisionConfigurationPresent: &no, DolbyVisionRPUPresent: &no}, &no},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := observedHDR(tc.video)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

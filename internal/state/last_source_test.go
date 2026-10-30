package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLastSourcePersistsScopedFactsAndClearsUnknown(t *testing.T) {
	s, _ := newTestState(t)
	key := core.MovieKey(1)
	hdr := true
	source := core.PlaybackSourceObservation{Revision: "src-123", ReleaseID: "private-release", VideoCodec: "hevc", Height: 2160, HDR: &hdr}
	if err := s.RecordLastSource(context.Background(), 1, key, source); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastSource(context.Background(), 1, key)
	sum := sha256.Sum256([]byte(source.ReleaseID))
	if err != nil || got == nil || got.ReleaseDigest != hex.EncodeToString(sum[:]) || got.VideoCodec != "hevc" || got.Height != 2160 || got.HDR == nil || !*got.HDR {
		t.Fatalf("stored source: %+v %v", got, err)
	}
	if other, err := s.LastSource(context.Background(), 2, key); err != nil || other != nil {
		t.Fatalf("cross viewer: %+v %v", other, err)
	}
	if err := s.RecordLastSource(context.Background(), 1, key, core.PlaybackSourceObservation{}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LastSource(context.Background(), 1, key); err != nil || got != nil {
		t.Fatalf("unknown must clear old facts: %+v %v", got, err)
	}
}

func TestLastSourceCanceledWhileDatabaseBusyDoesNotCommit(t *testing.T) {
	s, _ := newTestState(t)
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if os.Getenv(store.TestPostgresEnv) != "" {
		if _, err := tx.ExecContext(context.Background(), `LOCK TABLE state_source_sequence IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = s.RecordLastSource(ctx, 1, core.MovieKey(1), core.PlaybackSourceObservation{Revision: "src-123", VideoCodec: "h264"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked transaction did not cancel: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastSource(context.Background(), 1, core.MovieKey(1))
	if err != nil || got != nil {
		t.Fatalf("canceled source committed: %+v %v", got, err)
	}
}

func TestLastSourceDelayedEarlierTransactionCannotOverwriteNewer(t *testing.T) {
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("concurrent transactions require PostgreSQL")
	}
	s, _ := newTestState(t)
	key := core.MovieKey(1)
	allocated, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.recordLastSource(context.Background(), 1, key, core.PlaybackSourceObservation{Revision: "older", VideoCodec: "h264"}, func() error {
			close(allocated)
			<-release
			return nil
		})
	}()
	<-allocated
	if err := s.RecordLastSource(context.Background(), 1, key, core.PlaybackSourceObservation{Revision: "newer", VideoCodec: "hevc"}); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := s.LastSource(context.Background(), 1, key)
	if err != nil || got == nil || got.Revision != "newer" || got.VideoCodec != "hevc" {
		t.Fatalf("earlier transaction overwrote source: %+v %v", got, err)
	}
}

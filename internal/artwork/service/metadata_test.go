package service

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestArtworkMetadataQueuedAndPersisted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	active := func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }
	q := NewMetadataQueue(ctx, db, active, func(_ context.Context, _ frontend.ArtworkRequest, _ frontend.Image, _ frontend.ArtworkTransform) ([]byte, string, error) {
		calls.Add(1)
		close(started)
		<-release
		return encoded.Bytes(), "image/png", nil
	})
	req := frontend.ArtworkRequest{Viewer: &core.User{ID: 1}, Item: core.MovieKey(1)}
	ref := frontend.Image{Kind: "poster", Path: "opaque", Tag: "v1"}
	got := q.Get(ctx, req, ref)
	if got.Width != 0 || got.BlurHash != "" || got.ThumbHash != "" {
		t.Fatalf("cold read fabricated metadata: %+v", got)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	for i := 0; i < 5; i++ {
		q.Get(ctx, req, ref)
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate jobs: %d", calls.Load())
	}
	// A second node observes the shared lease and must not fetch the same image.
	q2 := NewMetadataQueue(ctx, db, active, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		t.Error("cached metadata fetched again")
		return nil, "", core.ErrNotAvailable
	})
	if err := q2.compute(ctx, artworkMetadataJob{key: artworkMetadataKey(req, ref), request: req, image: ref}); err == nil {
		t.Fatal("second worker acquired live claim")
	}
	close(release)
	deadline := time.After(2 * time.Second)
	for {
		got = q.Get(ctx, req, ref)
		if got.BlurHash != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("metadata not persisted")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got.Width != 8 || got.Height != 6 || len(got.BlurHash) != 28 || got.ThumbHash == "" {
		t.Fatalf("wrong metadata: %+v", got)
	}
	// A new node loads the persisted metadata in its worker, without any
	// request-path DB call or source fetch.
	for {
		again := q2.Get(ctx, req, ref)
		if again.BlurHash != "" {
			if again != got {
				t.Fatalf("persisted mismatch: %+v", again)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("persisted metadata not loaded")
		case <-time.After(5 * time.Millisecond):
		}
	}
	stale := ref
	stale.Tag = "stale-lease"
	key := artworkMetadataKey(req, stale)
	if _, err := db.ExecContext(ctx, `INSERT INTO artwork_image_metadata_claims(image_key,token,expires_at) VALUES(?,?,?) ON CONFLICT(image_key) DO UPDATE SET token=excluded.token,expires_at=excluded.expires_at`, key, "stale-worker", time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	q3 := NewMetadataQueue(ctx, db, active, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		return encoded.Bytes(), "image/png", nil
	})
	if err := q3.compute(ctx, artworkMetadataJob{key: key, request: req, image: stale}); err != nil {
		t.Fatalf("expired claim did not recover: %v", err)
	}
	changed := ref
	changed.Tag = "v2"
	if artworkMetadataKey(req, ref) == artworkMetadataKey(req, changed) {
		t.Fatal("version did not change metadata identity")
	}
	otherViewer := req
	otherViewer.Viewer = &core.User{ID: 2}
	otherItem := req
	otherItem.Item = core.MovieKey(2)
	channel := req
	channel.Item = 0
	channel.Channel = "different-channel"
	otherDevice := req
	otherDevice.Device.ID = "tablet"
	otherDevice.Device.Language = "fr"
	base := artworkMetadataKey(req, ref)
	for _, scoped := range []frontend.ArtworkRequest{otherViewer, otherItem, channel} {
		if artworkMetadataKey(scoped, ref) == base {
			t.Fatal("metadata identity lost viewer, item or channel scope")
		}
	}
	if artworkMetadataKey(otherDevice, ref) != base {
		t.Fatal("identical descriptor was not shared across devices")
	}
	if shared := q2.Get(ctx, otherDevice, ref); shared != got {
		t.Fatalf("cross-device metadata was not reused: %+v", shared)
	}
}

func TestArtworkMetadataBoundsAndViewerRevalidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	q := NewMetadataQueue(ctx, db, func(context.Context, int64) (*core.User, error) { return nil, core.ErrUnauthorized }, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		t.Fatal("inactive viewer fetched")
		return nil, "", nil
	})
	job := artworkMetadataJob{request: frontend.ArtworkRequest{Viewer: &core.User{ID: 3}}}
	if err := q.compute(ctx, job); err == nil {
		t.Fatal("inactive viewer accepted")
	}
	var oversized bytes.Buffer
	if err := png.Encode(&oversized, image.NewNRGBA(image.Rect(0, 0, 4001, 1))); err != nil {
		t.Fatal(err)
	}
	q.active = func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }
	q.read = func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		return oversized.Bytes(), "image/png", nil
	}
	if err := q.compute(ctx, job); err == nil {
		t.Fatal("oversized dimensions were decoded")
	}
}

func TestArtworkMetadataRequestDoesNotWaitForDatabase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	// Keep SQLite's sole connection occupied. The background worker may wait,
	// but request-time metadata lookup must not touch the database.
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	q := NewMetadataQueue(ctx, db, func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		return nil, "", core.ErrNotAvailable
	})
	done := make(chan frontend.Image, 1)
	go func() {
		done <- q.Get(ctx, frontend.ArtworkRequest{Viewer: &core.User{ID: 1}, Item: core.MovieKey(1)}, frontend.Image{Kind: "poster", Path: "opaque"})
	}()
	select {
	case got := <-done:
		if got.BlurHash != "" {
			t.Fatalf("cold metadata: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("request waited on database")
	}
}

// OpenTest uses a fresh PostgreSQL schema when ETHER_TEST_POSTGRES is set.
// Simulate another node publishing after the initial lookup but before this
// worker's post-claim EXISTS check, and ensure no second image fetch occurs.
func TestArtworkMetadataPostClaimExistingRow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	ddl := `CREATE TRIGGER metadata_peer_insert AFTER INSERT ON artwork_image_metadata_claims BEGIN INSERT INTO artwork_image_metadata(image_key,width,height,blurhash,thumbhash) VALUES(NEW.image_key,8,6,'peer-hash','peer-thumb'); END;`
	if store.DialectOf(db) == store.Postgres {
		ddl = `CREATE FUNCTION metadata_peer_insert_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO artwork_image_metadata(image_key,width,height,blurhash,thumbhash) VALUES(NEW.image_key,8,6,'peer-hash','peer-thumb'); RETURN NEW; END; $$; CREATE TRIGGER metadata_peer_insert AFTER INSERT ON artwork_image_metadata_claims FOR EACH ROW EXECUTE FUNCTION metadata_peer_insert_fn();`
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	q := NewMetadataQueue(ctx, db, func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		t.Fatal("existing metadata fetched again")
		return nil, "", nil
	})
	req := frontend.ArtworkRequest{Viewer: &core.User{ID: 1}, Item: core.MovieKey(1)}
	img := frontend.Image{Kind: "poster", Path: "opaque", Tag: "v1"}
	if err := q.compute(ctx, artworkMetadataJob{key: artworkMetadataKey(req, img), request: req, image: img}); err != nil {
		t.Fatalf("post-claim cached row: %v", err)
	}
}

func TestArtworkMetadataUpgradesExistingBlurHash(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	req := frontend.ArtworkRequest{Viewer: &core.User{ID: 1}, Item: core.MovieKey(1)}
	ref := frontend.Image{Kind: "poster", Path: "opaque", Tag: "v1"}
	key := artworkMetadataKey(req, ref)
	if _, err := db.ExecContext(ctx, `INSERT INTO artwork_image_metadata(image_key,width,height,blurhash) VALUES(?,?,?,?)`, key, 1, 1, "existing-blur"); err != nil {
		t.Fatal(err)
	}
	q := NewMetadataQueue(ctx, db, func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		return encoded.Bytes(), "image/png", nil
	})
	if err := q.compute(ctx, artworkMetadataJob{key: key, request: req, image: ref}); err != nil {
		t.Fatal(err)
	}
	metadata, err := q.load(ctx, key)
	if err != nil || metadata.blurhash != "existing-blur" || metadata.thumbhash != "1fsrB38I9wiIh4hwj3CI+AiIgIAICIgA" {
		t.Fatalf("upgrade lost old metadata or produced incorrect ThumbHash: %+v %v", metadata, err)
	}
}

func TestArtworkMetadataKeepsExistingBlurHashWhenUpgradeFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	req := frontend.ArtworkRequest{Viewer: &core.User{ID: 1}, Item: core.MovieKey(1)}
	ref := frontend.Image{Kind: "poster", Path: "opaque", Tag: "v1"}
	if _, err := db.ExecContext(ctx, `INSERT INTO artwork_image_metadata(image_key,width,height,blurhash) VALUES(?,?,?,?)`, artworkMetadataKey(req, ref), 8, 6, "existing-blur"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	q := NewMetadataQueue(ctx, db, func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		calls.Add(1)
		return nil, "", core.ErrNotAvailable
	})
	got := q.Get(ctx, req, ref)
	if got.BlurHash != "existing-blur" || got.Width != 8 || got.Height != 6 || got.ThumbHash != "" {
		t.Fatalf("stored BlurHash was not joined: %+v", got)
	}
	deadline := time.After(2 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("thumbhash upgrade was not attempted")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upgrade fetched more than once: %d", calls.Load())
	}
	again := q.Get(ctx, req, ref)
	if again.BlurHash != "existing-blur" || calls.Load() != 1 {
		t.Fatalf("failed upgrade changed the stored row: %+v calls=%d", again, calls.Load())
	}
}

func TestArtworkMetadataDoesNotBlockWithoutADeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	started := make(chan struct{})
	release := make(chan struct{})
	q := NewMetadataQueue(ctx, db, func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		close(started)
		<-release
		return nil, "", core.ErrNotAvailable
	})
	req := frontend.ArtworkRequest{Viewer: &core.User{ID: 1}, Item: core.MovieKey(1)}
	start := time.Now()
	got := q.Get(ctx, req, frontend.Image{Kind: frontend.ImagePerson, Path: "portrait", Subject: "tmdb:1"})
	if time.Since(start) > 200*time.Millisecond || got.BlurHash != "" || got.ThumbHash != "" {
		t.Fatalf("person image waited on hashing: %+v after %s", got, time.Since(start))
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("hash was not queued")
	}
	close(release)
}

func TestArtworkMetadataHashesInsideTheRequestBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	q := NewMetadataQueue(ctx, db, func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
		return encoded.Bytes(), "image/png", nil
	})
	req := frontend.ArtworkRequest{Viewer: &core.User{ID: 1}, Item: core.MovieKey(1)}
	ref := frontend.Image{Kind: "poster", Path: "opaque", Tag: "v1"}
	budget := frontend.WithSynchronousArtworkMetadata(ctx, time.Second)
	got := q.Get(budget, req, ref)
	if got.Width != 8 || got.Height != 6 || len(got.BlurHash) != 28 || got.ThumbHash == "" {
		t.Fatalf("request budget did not hash the poster: %+v", got)
	}
}

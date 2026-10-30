package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"image"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("artwork_metadata", 1, `CREATE TABLE artwork_image_metadata (image_key TEXT PRIMARY KEY, width INTEGER NOT NULL, height INTEGER NOT NULL, blurhash TEXT NOT NULL);
CREATE TABLE artwork_image_metadata_claims (image_key TEXT PRIMARY KEY, token TEXT NOT NULL, expires_at INTEGER NOT NULL);`)
	store.Register("artwork_metadata", 2, `ALTER TABLE artwork_image_metadata ADD COLUMN thumbhash TEXT NOT NULL DEFAULT '';`)
}

// artworkMetadataQueue serves only memory hits during requests. Missing
// descriptors schedule bounded, coalesced database and source work.
type artworkMetadataQueue struct {
	ctx     context.Context
	db      *sql.DB
	active  func(context.Context, int64) (*core.User, error)
	read    func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error)
	jobs    chan artworkMetadataJob
	mu      sync.Mutex
	cache   map[string]artworkMetadata
	pending map[string]bool
	retry   map[string]time.Time
}
type artworkMetadata struct {
	width, height int
	blurhash      string
	thumbhash     string
}
type artworkMetadataJob struct {
	key     string
	request frontend.ArtworkRequest
	image   frontend.Image
}

func newArtworkMetadataQueue(ctx context.Context, db *sql.DB, active func(context.Context, int64) (*core.User, error), read func(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error)) *artworkMetadataQueue {
	q := &artworkMetadataQueue{ctx: ctx, db: db, active: active, read: read, jobs: make(chan artworkMetadataJob, 128), cache: map[string]artworkMetadata{}, pending: map[string]bool{}, retry: map[string]time.Time{}}
	for i := 0; i < 2; i++ {
		go q.run()
	}
	return q
}

func artworkMetadataKey(req frontend.ArtworkRequest, img frontend.Image) string {
	h := sha256.New()
	viewer := int64(0)
	if req.Viewer != nil {
		viewer = req.Viewer.ID
	}
	// Personalized overlays can render differently for the same underlying
	// descriptor, so keep the viewer scope even for other artwork providers.
	// Variant/language selection must be represented by the descriptor tag;
	// identical descriptors share metadata across a viewer's devices.
	for _, part := range []string{"blurhash-v1", strconv.FormatInt(viewer, 10), req.Item.String(), req.Channel,
		img.Kind, img.Path, img.Tag, img.Subject} {
		// Length prefix prevents ambiguous identities even for unusual strings.
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{':'})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (q *artworkMetadataQueue) get(ctx context.Context, req frontend.ArtworkRequest, img frontend.Image) frontend.Image {
	if q == nil || q.db == nil || q.active == nil || q.read == nil || req.Viewer == nil || q.ctx.Err() != nil {
		return img
	}
	key := artworkMetadataKey(req, img)
	q.mu.Lock()
	defer q.mu.Unlock()
	if cached, ok := q.cache[key]; ok {
		img.Width, img.Height, img.BlurHash, img.ThumbHash = cached.width, cached.height, cached.blurhash, cached.thumbhash
		if cached.thumbhash != "" {
			return img
		}
	}
	if ctx.Err() != nil {
		return img
	}
	if q.pending[key] || time.Now().Before(q.retry[key]) || len(q.pending) >= cap(q.jobs) {
		return img
	}
	// Only the ID is retained. Current membership and policy are checked by
	// the worker before any bytes are fetched.
	req.Viewer = &core.User{ID: req.Viewer.ID}
	select {
	case q.jobs <- artworkMetadataJob{key: key, request: req, image: img}:
		q.pending[key] = true
	default:
	}
	return img
}

func (q *artworkMetadataQueue) run() {
	for {
		select {
		case <-q.ctx.Done():
			return
		case job := <-q.jobs:
			ctx, cancel := context.WithTimeout(q.ctx, 30*time.Second)
			err := q.compute(ctx, job)
			metadata, loadErr := q.load(ctx, job.key)
			if err == nil {
				err = loadErr
			}
			cancel()
			q.mu.Lock()
			delete(q.pending, job.key)
			if err != nil && q.ctx.Err() == nil {
				if len(q.retry) >= 1024 {
					for key, until := range q.retry {
						if !time.Now().Before(until) {
							delete(q.retry, key)
						}
					}
				}
				if len(q.retry) < 1024 {
					q.retry[job.key] = time.Now().Add(time.Minute)
				}
			} else {
				delete(q.retry, job.key)
			}
			if loadErr == nil {
				if len(q.cache) >= 4096 {
					q.cache = map[string]artworkMetadata{}
				}
				q.cache[job.key] = metadata
			}
			q.mu.Unlock()
		}
	}
}

func (q *artworkMetadataQueue) load(ctx context.Context, key string) (artworkMetadata, error) {
	var m artworkMetadata
	err := q.db.QueryRowContext(ctx, `SELECT width,height,blurhash,thumbhash FROM artwork_image_metadata WHERE image_key=?`, key).Scan(&m.width, &m.height, &m.blurhash, &m.thumbhash)
	return m, err
}

func (q *artworkMetadataQueue) compute(ctx context.Context, job artworkMetadataJob) error {
	viewer, err := q.active(ctx, job.request.Viewer.ID)
	if err != nil || viewer == nil || viewer.ID != job.request.Viewer.ID {
		return core.ErrUnauthorized
	}
	job.request.Viewer = viewer
	if metadata, err := q.load(ctx, job.key); err == nil && metadata.thumbhash != "" {
		return nil
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}
	// A shared lease prevents two nodes warming the same variant concurrently.
	// It outlives the bounded job timeout and recovers after a crashed worker.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(nonce[:])
	now := time.Now().Unix()
	claim, err := q.db.ExecContext(ctx, `INSERT INTO artwork_image_metadata_claims(image_key,token,expires_at) VALUES(?,?,?) ON CONFLICT(image_key) DO UPDATE SET token=excluded.token,expires_at=excluded.expires_at WHERE artwork_image_metadata_claims.expires_at<?`, job.key, token, now+60, now)
	if err != nil {
		return err
	}
	claimed, err := claim.RowsAffected()
	if err != nil {
		return err
	}
	if claimed == 0 {
		return core.ErrNotAvailable
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = q.db.ExecContext(cleanup, `DELETE FROM artwork_image_metadata_claims WHERE image_key=? AND token=?`, job.key, token)
	}()
	if metadata, err := q.load(ctx, job.key); err == nil && metadata.thumbhash != "" {
		return nil
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}
	data, _, err := q.read(ctx, job.request, job.image, frontend.ArtworkTransform{})
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > 15<<20 {
		return core.ErrNotAvailable
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4000 || cfg.Height > 4000 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return core.ErrNotAvailable
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	hash, err := artwork.BlurHash(decoded, 4, 3)
	if err != nil {
		return err
	}
	thumb, err := artwork.ThumbHash(decoded)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO artwork_image_metadata(image_key,width,height,blurhash,thumbhash) VALUES(?,?,?,?,?) ON CONFLICT(image_key) DO UPDATE SET thumbhash=excluded.thumbhash WHERE artwork_image_metadata.thumbhash=''`, job.key, cfg.Width, cfg.Height, hash, base64.StdEncoding.EncodeToString(thumb))
	return err
}

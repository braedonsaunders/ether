package onair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/store"
	"golang.org/x/image/font/opentype"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const assetLimit = 32 << 20

func (s *Service) putAsset(ctx context.Context, name, kind, license, mime string, b []byte) (Asset, error) {
	sum := sha256.Sum256(b)
	a := Asset{Digest: hex.EncodeToString(sum[:]), Name: name, Kind: kind, License: license, Size: int64(len(b)), MIME: mime, UsedBy: []string{}}
	a.URL = "/ether/api/onair/assets/" + a.Digest
	_, e := s.db.ExecContext(ctx, "INSERT INTO onair_assets(digest,kind,name,mime,size,license,content,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(digest) DO NOTHING", a.Digest, kind, name, mime, a.Size, license, b, time.Now().Unix())
	if e != nil {
		return a, e
	}
	e = s.db.QueryRowContext(ctx, "SELECT name,kind,size,license,mime FROM onair_assets WHERE digest=?", a.Digest).Scan(&a.Name, &a.Kind, &a.Size, &a.License, &a.MIME)
	return a, e
}
func (s *Service) UploadAsset(ctx context.Context, name, kind, license string, r io.Reader) (Asset, error) {
	b, e := io.ReadAll(io.LimitReader(r, assetLimit+1))
	if e != nil {
		return Asset{}, e
	}
	if len(b) == 0 || len(b) > assetLimit {
		return Asset{}, fmt.Errorf("asset must be between 1 byte and 32 MB")
	}
	mime := http.DetectContentType(b)
	switch kind {
	case "font":
		if strings.TrimSpace(license) == "" {
			return Asset{}, fmt.Errorf("font licence required")
		}
		if _, e = opentype.Parse(b); e != nil {
			return Asset{}, e
		}
		mime = "font/ttf"
	case "image":
		if strings.Contains(strings.ToLower(string(b[:min(512, len(b))])), "<svg") {
			_, _, e = artwork.NewSVGRasterizer().Rasterize(ctx, "onair-upload", b, artwork.RasterOptions{})
			mime = "image/svg+xml"
		} else {
			var c image.Config
			c, _, e = image.DecodeConfig(bytes.NewReader(b))
			if e == nil && (c.Width < 1 || c.Height < 1 || c.Width > 4096 || c.Height > 4096 || int64(c.Width)*int64(c.Height) > 16<<20) {
				e = fmt.Errorf("image exceeds 4096 pixels or 16 megapixels")
			}
			if e == nil {
				_, _, e = image.Decode(bytes.NewReader(b))
			}
		}
		if e != nil {
			return Asset{}, e
		}
	case "audio", "video":
		if s.o.Probe == nil {
			return Asset{}, fmt.Errorf("media probe unavailable")
		}
		f, e := os.CreateTemp(s.o.Dir, ".probe-*")
		if e != nil {
			return Asset{}, e
		}
		defer os.Remove(f.Name())
		if _, e = f.Write(b); e != nil {
			f.Close()
			return Asset{}, e
		}
		f.Close()
		// Lossless owner stings have a separate, explicitly selected demuxer.
		// The general media probe whitelist remains unchanged.
		if kind == "audio" {
			format := ""
			if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE" {
				format = "wav"
			}
			if len(b) >= 12 && string(b[:4]) == "FORM" && (string(b[8:12]) == "AIFF" || string(b[8:12]) == "AIFC") {
				format = "aiff"
			}
			if len(b) >= 4 && string(b[:4]) == "fLaC" {
				format = "flac"
			}
			if format != "" {
				if s.o.FFmpeg == nil {
					return Asset{}, fmt.Errorf("audio conversion unavailable")
				}
				out := f.Name() + ".m4a"
				defer os.Remove(out)
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(cctx, s.o.FFmpeg(), "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-protocol_whitelist", "file,pipe", "-format_whitelist", format, "-f", format, "-i", f.Name(), "-map", "0:a:0", "-vn", "-t", "31", "-ac", "2", "-ar", "48000", "-c:a", "aac", "-b:a", "192k", "-threads", "1", out)
				if e = cmd.Run(); e != nil {
					return Asset{}, fmt.Errorf("convert audio asset: %w", e)
				}
				b, e = os.ReadFile(out)
				if e != nil {
					return Asset{}, e
				}
				if e = os.WriteFile(f.Name(), b, 0600); e != nil {
					return Asset{}, e
				}
				mime = "audio/mp4"
			}
		}
		info, e := s.o.Probe(ctx, f.Name())
		if e != nil {
			return Asset{}, e
		}
		if info.Duration <= 0 || info.Duration > 30*time.Second {
			return Asset{}, fmt.Errorf("asset duration must be at most 30 seconds")
		}
		found := false
		for _, st := range info.Streams {
			if st.Type == kind {
				found = true
			}
		}
		if !found {
			return Asset{}, fmt.Errorf("asset has no %s stream", kind)
		}
	default:
		return Asset{}, fmt.Errorf("unknown asset kind")
	}
	return s.putAsset(ctx, name, kind, license, mime, b)
}
func (s *Service) Assets(ctx context.Context) ([]Asset, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT digest,name,kind,size,license,mime,source,attribution FROM onair_assets ORDER BY created_at")
	if e != nil {
		return nil, e
	}
	out := []Asset{}
	for rows.Next() {
		var a Asset
		if e = rows.Scan(&a.Digest, &a.Name, &a.Kind, &a.Size, &a.License, &a.MIME, &a.Source, &a.Attribution); e != nil {
			rows.Close()
			return nil, e
		}
		a.URL = "/ether/api/onair/assets/" + a.Digest
		out = append(out, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for i := range out {
		out[i].UsedBy, e = s.assetUsage(ctx, out[i].Digest)
		if e != nil {
			return nil, e
		}
	}
	return s.importAssets(ctx, out)
}
func (s *Service) assetUsage(ctx context.Context, d string) ([]string, error) {
	query := `SELECT id FROM onair_looks WHERE data LIKE ? UNION SELECT id FROM onair_families WHERE data LIKE ? UNION SELECT reel_id FROM onair_reel_clips WHERE source_digest=?`
	args := []any{"%" + d + "%", "%" + d + "%", d}
	channels, err := s.hasChannelAssets(ctx)
	if err != nil {
		return nil, err
	}
	if channels {
		query += ` UNION SELECT id FROM livetv_channels WHERE data LIKE ?`
		args = append(args, "%"+d+"%")
	}
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (s *Service) DeleteAsset(ctx context.Context, d string) error {
	pending, e := s.db.ExecContext(ctx, "DELETE FROM onair_imports WHERE id=? AND reel_id=''", d)
	if e != nil {
		return e
	}
	if n, _ := pending.RowsAffected(); n > 0 {
		return nil
	}
	s.assetMu.Lock()
	defer s.assetMu.Unlock()
	query := `DELETE FROM onair_assets WHERE digest=? AND NOT EXISTS(SELECT 1 FROM onair_looks WHERE data LIKE ?) AND NOT EXISTS(SELECT 1 FROM onair_families WHERE data LIKE ?) AND NOT EXISTS(SELECT 1 FROM onair_reel_clips WHERE source_digest=?)`
	args := []any{d, "%" + d + "%", "%" + d + "%", d}
	channels, err := s.hasChannelAssets(ctx)
	if err != nil {
		return err
	}
	if channels {
		query += ` AND NOT EXISTS(SELECT 1 FROM livetv_channels WHERE data LIKE ?)`
		args = append(args, "%"+d+"%")
	}
	res, e := s.db.ExecContext(ctx, query, args...)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e == nil && n == 0 {
		u, e := s.assetUsage(ctx, d)
		if e != nil {
			return e
		}
		if len(u) > 0 {
			return ErrUsed
		}
		return ErrNotFound
	}
	return e
}

// putReelAsset serializes aggregate budget admission across database nodes, not
// merely goroutines. The singleton row is a write lock on SQLite and PostgreSQL.
func (s *Service) putReelAsset(ctx context.Context, name string, b []byte) (Asset, error) {
	sum := sha256.Sum256(b)
	a := Asset{Digest: hex.EncodeToString(sum[:]), Name: name, Kind: "reel", Size: int64(len(b)), MIME: "video/mp4", UsedBy: []string{}}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return a, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "INSERT INTO onair_settings(k,v) VALUES('reel_budget','1') ON CONFLICT(k) DO NOTHING"); e != nil {
		return a, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE onair_settings SET v='1' WHERE k='reel_budget'"); e != nil {
		return a, e
	}
	var used, exists int64
	if e = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(size),0) FROM onair_assets WHERE kind='reel'").Scan(&used); e != nil {
		return a, e
	}
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM onair_assets WHERE digest=? AND kind='reel'", a.Digest).Scan(&exists); e != nil {
		return a, e
	}
	if exists == 0 && used+a.Size > 5<<30 {
		return a, fmt.Errorf("5 GB reel budget exceeded")
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO onair_assets(digest,kind,name,mime,size,license,content,created_at) VALUES(?,'reel',?,'video/mp4',?,'',?,?) ON CONFLICT(digest) DO UPDATE SET kind='reel'", a.Digest, name, a.Size, b, time.Now().Unix()); e != nil {
		return a, e
	}
	return a, tx.Commit()
}

// On Air can run independently of Live TV (including plugin test databases).
func (s *Service) hasChannelAssets(ctx context.Context) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='livetv_channels')`
	if store.DialectOf(s.db) == store.Postgres {
		query = `SELECT to_regclass('livetv_channels') IS NOT NULL`
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, query).Scan(&exists)
	return exists, err
}

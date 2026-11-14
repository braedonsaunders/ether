package vela

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"time"
)

func init() {
	frontend.RegisterMigration("vela_replays", 1, `CREATE TABLE vela_replays (key TEXT PRIMARY KEY, digest TEXT NOT NULL, response BLOB, expires INTEGER NOT NULL); CREATE INDEX vela_replays_expiry ON vela_replays(expires);`)
}

type savedResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

func encryption(key []byte) (cipher.AEAD, error) {
	c, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(c)
}

// claim persists before admission. A process interrupted during a mutation leaves
// a pending claim; retries fail closed instead of admitting duplicate playback.
func (i *instance) claim(ctx context.Context, key, digest string) (*savedResponse, bool, error) {
	if i.db == nil || i.seal == nil {
		return nil, false, errors.New("mutation persistence unavailable")
	}
	now := time.Now().UnixMilli()
	_, e := i.db.ExecContext(ctx, `DELETE FROM vela_replays WHERE key IN (SELECT key FROM vela_replays WHERE expires<=? LIMIT 100)`, now)
	if e != nil {
		return nil, false, e
	}
	res, e := i.db.ExecContext(ctx, `INSERT INTO vela_replays(key,digest,expires) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`, key, digest, now+(24*time.Hour).Milliseconds())
	if e != nil {
		return nil, false, e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return nil, false, e
	}
	if n == 1 {
		return nil, false, nil
	}
	var stored string
	var data []byte
	e = i.db.QueryRowContext(ctx, `SELECT digest,response FROM vela_replays WHERE key=?`, key).Scan(&stored, &data)
	if e != nil {
		return nil, false, e
	}
	if stored != digest {
		return nil, true, nil
	}
	if len(data) < i.seal.NonceSize() {
		return nil, false, errPending
	}
	plain, e := i.seal.Open(nil, data[:i.seal.NonceSize()], data[i.seal.NonceSize():], []byte(key))
	if e != nil {
		return nil, false, e
	}
	var out savedResponse
	e = json.Unmarshal(plain, &out)
	return &out, false, e
}

var errPending = errors.New("mutation outcome pending")

func (i *instance) save(ctx context.Context, key string, out savedResponse) error {
	b, e := json.Marshal(out)
	if e != nil {
		return e
	}
	nonce := make([]byte, i.seal.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	data := i.seal.Seal(nonce, nonce, b, []byte(key))
	_, e = i.db.ExecContext(ctx, `UPDATE vela_replays SET response=? WHERE key=?`, data, key)
	return e
}

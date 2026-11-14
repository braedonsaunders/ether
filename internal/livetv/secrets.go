package livetv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

// Source secrets are sealed inside the stored source JSON
// (config.FieldSealer): the playlist and guide URLs (Xtream URLs and many
// IPTV links embed credentials) are bound to the source id; the username,
// password, token and custom header values are also bound to where they
// are sent — the auth type, Xtream server and the URLs' origins — so a
// stored source repointed elsewhere cannot use them. Scheduling queries
// only test the URLs for emptiness, which sealing preserves.

// errSourceSecretMoved is returned when an edit would carry redacted
// credentials to another provider.
var errSourceSecretMoved = errors.New("re-enter the source's credentials when changing its authentication type, server or URLs")

type sourceField struct {
	name  string
	p     *string
	bound bool // bound to the destination, not just the source id
}

func sourceFields(src *Source) []sourceField {
	fs := []sourceField{{"playlistUrl", &src.PlaylistURL, false}, {"guideUrl", &src.GuideURL, false}}
	if a := src.Auth; a != nil {
		fs = append(fs, sourceField{"auth.username", &a.Username, true}, sourceField{"auth.password", &a.Password, true}, sourceField{"auth.token", &a.Token, true})
	}
	for i := range src.Logins {
		l := &src.Logins[i]
		fs = append(fs, sourceField{"login." + l.ID + ".url", &l.PlaylistURL, false}, sourceField{"login." + l.ID + ".username", &l.Username, true}, sourceField{"login." + l.ID + ".password", &l.Password, true})
	}
	return fs
}

// urlOrigin is the scheme and host of a URL ("" when it has none).
func urlOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// sourceDestination says where a source's credentials are sent. It reads
// only plaintext, non-secret parts: URLs must already be open.
func sourceDestination(src *Source) string {
	typ, server := "", ""
	if src.Auth != nil {
		typ, server = src.Auth.Type, urlOrigin(src.Auth.Server)
	}
	return strings.Join([]string{typ, server, urlOrigin(src.PlaylistURL), urlOrigin(src.GuideURL)}, "\x00")
}

func sourceContext(src *Source, field string, bound bool) []string {
	ctx := []string{"livetv_sources", field, src.ID}
	if bound {
		ctx = append(ctx, sourceDestination(src))
	}
	return ctx
}

func copyAuth(src *Source) {
	src.Logins = append([]ProviderLogin(nil), src.Logins...)
	if src.Auth == nil {
		return
	}
	a := *src.Auth
	if src.Auth.Headers != nil {
		a.Headers = make(map[string]string, len(src.Auth.Headers))
		for k, v := range src.Auth.Headers {
			a.Headers[k] = v
		}
	}
	src.Auth = &a
}

// sealSource returns the stored form of src (a copy).
func (s *Service) sealSource(src Source) (Source, error) {
	copyAuth(&src)
	seal := func(field, plain string, bound bool) (string, error) {
		v, err := config.SealWith(s.o.Sealer, plain, sourceContext(&src, field, bound)...)
		if err != nil {
			return "", fmt.Errorf("livetv: seal source %s: %w", src.ID, err)
		}
		return v, nil
	}
	// Destination-bound secrets first: their context uses the plaintext URLs.
	if a := src.Auth; a != nil {
		for _, k := range sortedHeaderKeys(a.Headers) {
			v, err := seal("auth.headers."+k, a.Headers[k], true)
			if err != nil {
				return Source{}, err
			}
			a.Headers[k] = v
		}
	}
	fs := sourceFields(&src)
	for i := len(fs) - 1; i >= 0; i-- {
		v, err := seal(fs[i].name, *fs[i].p, fs[i].bound)
		if err != nil {
			return Source{}, err
		}
		*fs[i].p = v
	}
	return src, nil
}

// openSource reverses sealSource; legacy reports plaintext secrets.
func (s *Service) openSource(src Source) (Source, bool, error) {
	copyAuth(&src)
	legacy := false
	open := func(field, stored string, bound bool) (string, error) {
		v, l, err := config.OpenWith(s.o.Sealer, stored, sourceContext(&src, field, bound)...)
		if err != nil {
			return "", fmt.Errorf("livetv: source %s: %w", src.ID, err)
		}
		legacy = legacy || l
		return v, nil
	}
	for _, f := range sourceFields(&src) {
		v, err := open(f.name, *f.p, f.bound)
		if err != nil {
			return Source{}, false, err
		}
		*f.p = v
	}
	if a := src.Auth; a != nil {
		for _, k := range sortedHeaderKeys(a.Headers) {
			v, err := open("auth.headers."+k, a.Headers[k], true)
			if err != nil {
				return Source{}, false, err
			}
			a.Headers[k] = v
		}
	}
	return src, legacy, nil
}

func sortedHeaderKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// encodeSource is the stored JSON of src.
func (s *Service) encodeSource(src Source) ([]byte, error) {
	sealed, err := s.sealSource(storedSource(src))
	if err != nil {
		return nil, err
	}
	return json.Marshal(sealed)
}

// decodeSource opens a stored source row.
func (s *Service) decodeSource(b []byte) (Source, bool, error) {
	var src Source
	if err := json.Unmarshal(b, &src); err != nil {
		return Source{}, false, err
	}
	return s.openSource(src)
}

// sourceMoved reports whether an edited source sends its credentials
// somewhere other than the stored one. URLs the owner API returned empty
// (Xtream-derived) are rederived from the server and do not count.
func sourceMoved(src *Source, old Source) bool {
	oldType, oldServer := "", ""
	if old.Auth != nil {
		oldType, oldServer = old.Auth.Type, urlOrigin(old.Auth.Server)
	}
	if src.Auth.Type != oldType || urlOrigin(src.Auth.Server) != oldServer {
		return true
	}
	if src.PlaylistURL != "" && urlOrigin(src.PlaylistURL) != urlOrigin(old.PlaylistURL) {
		return true
	}
	return src.GuideURL != "" && urlOrigin(src.GuideURL) != urlOrigin(old.GuideURL)
}

// MigrateSecrets seals source secrets stored in plaintext and returns how
// many rows it rewrote. A row is replaced only if unchanged since read
// (safe to rerun on any node); a row that does not open is left untouched
// and reported in the error.
func (s *Service) MigrateSecrets(ctx context.Context) (int, error) {
	if s.o.Sealer == nil {
		return 0, nil
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT id, data FROM livetv_sources ORDER BY id`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id  string
		raw []byte
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	batch := store.NewUpgradeBatch(s.o.DB)
	defer batch.Close()
	n, failed := 0, 0
	var firstErr error
	for i, r := range all {
		store.WorkProgress(ctx, "sources", i, len(all))
		src, legacy, err := s.decodeSource(r.raw)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !legacy {
			continue
		}
		sealed, err := s.sealSource(src)
		if err != nil {
			return n, err
		}
		b, err := json.Marshal(sealed)
		if err != nil {
			return n, err
		}
		if string(b) == string(r.raw) {
			continue
		}
		// Import stores data as bytes (a BLOB on SQLite), status updates
		// as text: compare as text.
		res, err := batch.ExecContext(ctx, `UPDATE livetv_sources SET data=? WHERE id=? AND CAST(data AS TEXT)=?`, b, r.id, string(r.raw))
		if err != nil {
			return n, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	if err := batch.Flush(); err != nil {
		return n, err
	}
	store.WorkProgress(ctx, "sources", len(all), len(all))
	if failed > 0 {
		return n, fmt.Errorf("livetv: %d stored source(s) could not be opened and were left unchanged: %w", failed, firstErr)
	}
	return n, nil
}

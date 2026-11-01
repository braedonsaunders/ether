package newznab

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// maxTorrentBytes bounds a downloaded .torrent document.
const maxTorrentBytes = 10 << 20

// errNotTorrent reports a .torrent download that was neither bencode nor a
// magnet redirect.
var errNotTorrent = errors.New("newznab: response is not a torrent file")

// NormalizeInfoHash returns a lower-case 40-character hex BitTorrent v1
// info hash, converting a 32-character base32 hash; "" when s is neither.
func NormalizeInfoHash(s string) string {
	s = strings.TrimSpace(s)
	switch len(s) {
	case 40:
		if _, err := hex.DecodeString(s); err == nil {
			return strings.ToLower(s)
		}
	case 32:
		if b, err := base32.StdEncoding.DecodeString(strings.ToUpper(s)); err == nil && len(b) == 20 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

// InfoHashFromMagnet extracts the v1 info hash from a magnet URI's
// xt=urn:btih: parameter; "" when absent.
func InfoHashFromMagnet(magnet string) string {
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		return ""
	}
	q := magnet[strings.IndexByte(magnet, ':')+1:]
	q = strings.TrimPrefix(q, "?")
	values, err := url.ParseQuery(q)
	if err != nil {
		// Tolerate unescaped junk in other parameters.
		values = url.Values{}
		for _, part := range strings.Split(q, "&") {
			if k, v, ok := strings.Cut(part, "="); ok {
				values.Add(k, v)
			}
		}
	}
	for _, xt := range values["xt"] {
		if lower := strings.ToLower(xt); strings.HasPrefix(lower, "urn:btih:") {
			if h := NormalizeInfoHash(xt[len("urn:btih:"):]); h != "" {
				return h
			}
		}
	}
	return ""
}

// MagnetFor builds a magnet URI for an info hash.
func MagnetFor(infoHash, name string, trackers []string) string {
	v := "magnet:?xt=urn:btih:" + infoHash
	if name != "" {
		v += "&dn=" + url.QueryEscape(name)
	}
	for _, tr := range trackers {
		v += "&tr=" + url.QueryEscape(tr)
	}
	return v
}

// TorrentMeta is what a .torrent file contributes to a release.
type TorrentMeta struct {
	InfoHash string
	Name     string
	Trackers []string
}

// ParseTorrent computes the info hash (SHA-1 of the bencoded info
// dictionary) and reads the name and trackers of a .torrent file.
func ParseTorrent(data []byte) (TorrentMeta, error) {
	d := &bdecoder{b: data}
	if d.peek() != 'd' {
		return TorrentMeta{}, errNotTorrent
	}
	d.pos++
	var meta TorrentMeta
	for d.peek() != 'e' {
		key, err := d.str()
		if err != nil {
			return TorrentMeta{}, err
		}
		start := d.pos
		switch string(key) {
		case "info":
			if err := d.skip(0); err != nil {
				return TorrentMeta{}, err
			}
			sum := sha1.Sum(data[start:d.pos])
			meta.InfoHash = hex.EncodeToString(sum[:])
			meta.Name = torrentName(data[start:d.pos])
		case "announce":
			v, err := d.str()
			if err != nil {
				return TorrentMeta{}, err
			}
			meta.Trackers = appendTracker(meta.Trackers, string(v))
		case "announce-list":
			tiers, err := d.list()
			if err != nil {
				return TorrentMeta{}, err
			}
			for _, tier := range tiers {
				if trs, ok := tier.([]any); ok {
					for _, tr := range trs {
						if s, ok := tr.(string); ok {
							meta.Trackers = appendTracker(meta.Trackers, s)
						}
					}
				}
			}
		default:
			if err := d.skip(0); err != nil {
				return TorrentMeta{}, err
			}
		}
	}
	if meta.InfoHash == "" {
		return TorrentMeta{}, fmt.Errorf("%w: no info dictionary", errNotTorrent)
	}
	return meta, nil
}

func appendTracker(list []string, tr string) []string {
	if tr == "" || len(list) >= 20 {
		return list
	}
	for _, x := range list {
		if x == tr {
			return list
		}
	}
	return append(list, tr)
}

func torrentName(info []byte) string {
	d := &bdecoder{b: info}
	if d.peek() != 'd' {
		return ""
	}
	d.pos++
	for d.peek() == '0' || (d.peek() >= '1' && d.peek() <= '9') {
		key, err := d.str()
		if err != nil {
			return ""
		}
		if string(key) == "name" {
			v, err := d.str()
			if err != nil {
				return ""
			}
			return string(v)
		}
		if err := d.skip(0); err != nil {
			return ""
		}
	}
	return ""
}

// bdecoder is a minimal bencode reader: enough to find the info dictionary
// and read string lists.
type bdecoder struct {
	b   []byte
	pos int
}

var errBencode = fmt.Errorf("%w: malformed bencode", errNotTorrent)

func (d *bdecoder) peek() byte {
	if d.pos >= len(d.b) {
		return 0
	}
	return d.b[d.pos]
}

func (d *bdecoder) str() ([]byte, error) {
	colon := bytes.IndexByte(d.b[d.pos:], ':')
	if colon <= 0 || colon > 10 {
		return nil, errBencode
	}
	n, err := strconv.Atoi(string(d.b[d.pos : d.pos+colon]))
	if err != nil || n < 0 {
		return nil, errBencode
	}
	start := d.pos + colon + 1
	if start+n > len(d.b) {
		return nil, errBencode
	}
	d.pos = start + n
	return d.b[start:d.pos], nil
}

func (d *bdecoder) list() ([]any, error) {
	v, err := d.value(0)
	if err != nil {
		return nil, err
	}
	l, ok := v.([]any)
	if !ok {
		return nil, errBencode
	}
	return l, nil
}

// value decodes strings and lists of strings (other values decode to nil).
func (d *bdecoder) value(depth int) (any, error) {
	if depth > 32 {
		return nil, errBencode
	}
	switch c := d.peek(); {
	case c == 'l':
		d.pos++
		var out []any
		for d.peek() != 'e' {
			if d.peek() == 0 {
				return nil, errBencode
			}
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		d.pos++
		return out, nil
	case c >= '0' && c <= '9':
		s, err := d.str()
		return string(s), err
	default:
		return nil, d.skip(depth)
	}
}

// skip advances past one value of any type.
func (d *bdecoder) skip(depth int) error {
	if depth > 64 {
		return errBencode
	}
	switch c := d.peek(); {
	case c == 'i':
		end := bytes.IndexByte(d.b[d.pos:], 'e')
		if end < 0 {
			return errBencode
		}
		d.pos += end + 1
		return nil
	case c == 'l' || c == 'd':
		d.pos++
		for d.peek() != 'e' {
			if d.peek() == 0 {
				return errBencode
			}
			if c == 'd' {
				if _, err := d.str(); err != nil {
					return err
				}
			}
			if err := d.skip(depth + 1); err != nil {
				return err
			}
		}
		d.pos++
		return nil
	case c >= '0' && c <= '9':
		_, err := d.str()
		return err
	}
	return errBencode
}

// knownTorrent completes a release that already identifies its torrent.
func knownTorrent(r core.Release) (core.Release, bool) {
	if h := NormalizeInfoHash(r.InfoHash); h != "" {
		r.InfoHash = h
		if r.MagnetURL == "" {
			r.MagnetURL = MagnetFor(h, r.Name, nil)
		}
		return r, true
	}
	if h := InfoHashFromMagnet(r.MagnetURL); h != "" {
		r.InfoHash = h
		return r, true
	}
	return r, false
}

// ResolveTorrent fills a torrent release's InfoHash and MagnetURL by
// downloading its .torrent link (counting as a grab) when the indexer only
// supplied a download URL. Releases that already carry both are returned
// unchanged without a request.
func (c *Client) ResolveTorrent(ctx context.Context, r core.Release) (core.Release, error) {
	out, _, err := c.resolveTorrentCached(ctx, r)
	return out, err
}

func (c *Client) resolveTorrentCached(ctx context.Context, r core.Release) (core.Release, bool, error) {
	if known, ok := knownTorrent(r); ok {
		return known, true, nil
	}
	key := torrentDescriptorKey(c.Config(), r)
	if hash, magnet, ok := c.torrents.get(key, c.now()); ok {
		r.InfoHash, r.MagnetURL = hash, magnet
		return r, true, nil
	}
	out, err := c.resolveTorrent(ctx, r)
	if err == nil && out.InfoHash != "" {
		c.torrents.put(key, out.InfoHash, out.MagnetURL, c.now())
	}
	return out, false, err
}

func (c *Client) resolveTorrent(ctx context.Context, r core.Release) (core.Release, error) {
	if c.Config().UsesBackend() {
		b, err := c.backend()
		if err != nil {
			return r, err
		}
		return b.ResolveTorrent(ctx, r, c.reserve)
	}
	if r.NZBURL == "" {
		return r, fmt.Errorf("newznab: torrent %q has no info hash, magnet or download URL: %w", r.Name, core.ErrReleaseRejected)
	}
	if err := c.reserve(ctx, "grab"); err != nil {
		return r, err
	}
	rawURL := r.NZBURL
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		base, err := url.Parse(strings.TrimRight(c.Config().URL, "/") + "/")
		if err != nil {
			return r, redactURLError(err)
		}
		ref, err := url.Parse(rawURL)
		if err != nil {
			return r, redactURLError(err)
		}
		rawURL = base.ResolveReference(ref).String()
	}
	// The download URL comes from the feed (untrusted): fetch it through
	// the safefetch guard, like FetchNZB. Only the owner-configured
	// indexer origin may be private; redirects away from it (other than
	// magnet, handled below) are revalidated at dial time.
	// Torznab download links commonly redirect to a magnet URI, which
	// net/http cannot follow: stop there and read the Location header.
	resp, err := c.requestWith(ctx, c.grabIdentity(r).torrent(), rawURL, func(next *http.Request, _ []*http.Request) error {
		if next.URL.Scheme == "magnet" {
			return http.ErrUseLastResponse
		}
		return nil
	})
	if err != nil {
		// url.Error embeds the URL, which can carry the indexer API key.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return r, fmt.Errorf("newznab: download torrent for %q: %w", r.Name, err)
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && strings.HasPrefix(strings.ToLower(loc), "magnet:") {
		if h := InfoHashFromMagnet(loc); h != "" {
			r.InfoHash, r.MagnetURL = h, loc
			return r, nil
		}
		return r, fmt.Errorf("newznab: torrent %q redirected to a magnet without an info hash: %w", r.Name, core.ErrReleaseRejected)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Honor the throttle like API requests do, so the next play does
		// not immediately hit the throttled indexer again.
		return r, c.recordRateLimit(ctx, resp.Header.Get("Retry-After"), apiError(429, "too many requests"))
	}
	if resp.StatusCode != http.StatusOK {
		return r, apiError(resp.StatusCode, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentBytes+1))
	if err != nil {
		return r, err
	}
	if len(body) > maxTorrentBytes {
		return r, fmt.Errorf("newznab: torrent exceeds %d bytes", maxTorrentBytes)
	}
	if trimmed := bytes.TrimSpace(body); bytes.HasPrefix(bytes.ToLower(trimmed), []byte("magnet:")) {
		if h := InfoHashFromMagnet(string(trimmed)); h != "" {
			r.InfoHash, r.MagnetURL = h, string(trimmed)
			return r, nil
		}
	}
	meta, err := ParseTorrent(body)
	if err != nil {
		if _, perr := parseFeed(body, c.Name()); perr != nil {
			var ae *APIError
			if errors.As(perr, &ae) {
				return r, perr
			}
		}
		return r, fmt.Errorf("newznab: torrent %q: %w", r.Name, err)
	}
	r.InfoHash = meta.InfoHash
	name := meta.Name
	if name == "" {
		name = r.Name
	}
	r.MagnetURL = MagnetFor(meta.InfoHash, name, meta.Trackers)
	return r, nil
}

// ResolveTorrent implements the debrid indexer's torrent resolution by
// routing to the release's originating indexer.
func (m *Manager) ResolveTorrent(ctx context.Context, r core.Release) (core.Release, error) {
	if known, ok := knownTorrent(r); ok {
		return known, nil
	}
	for _, cl := range m.Clients() {
		if cl.Name() == r.Indexer {
			started := time.Now()
			out, cached, err := cl.resolveTorrentCached(ctx, r)
			if !cached {
				m.emitEvent(Event{Indexer: cl.Name(), Kind: "grab", Success: err == nil, BudgetExhausted: errors.Is(err, core.ErrBudgetExhausted), Duration: time.Since(started), At: time.Now()})
			}
			return out, err
		}
	}
	return r, fmt.Errorf("newznab: indexer %q for torrent %q is no longer configured", r.Indexer, r.Name)
}

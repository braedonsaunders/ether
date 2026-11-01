package updater

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/schedule"
)

// DefaultFeedBase is where release manifests live when config has no
// override: <base>/<channel>.json (+ optional <channel>.json.sig). The
// release workflow keeps every channel's current manifest on the rolling
// "channels" release: GitHub's releases/latest never points at a beta, so a
// beta manifest cannot be found there.
const DefaultFeedBase = "https://github.com/braedonsaunders/ether/releases/download/channels"

// Options configures a Client. Zero values fall back to build defaults.
type Options struct {
	// FeedURL is the manifest URL, feed base, or a template containing
	// "{channel}". See ManifestURL for resolution. Empty: DefaultFeedBase.
	FeedURL string
	// Channel: "stable" (default) or "beta".
	Channel string
	// CurrentVersion is the running version. Empty: the build Version.
	CurrentVersion string
	// PublicKey verifies manifests. Nil: the compiled-in key (required).
	PublicKey ed25519.PublicKey
	// HTTPClient for feed + artifact fetches. Nil: a 30s-timeout client
	// for manifests; downloads set their own per-request deadline via ctx.
	HTTPClient *http.Client
	// GOOS/GOARCH override artifact selection (tests, cross-tooling).
	GOOS, GOARCH string
	// PreferKinds overrides artifact kind preference (e.g. installer-only
	// on macOS where the .dmg is the product).
	PreferKinds []string
	Logger      *slog.Logger
}

// Client checks, downloads and applies updates from the signed feed.
type Client struct {
	feedURL string
	channel string
	current string
	pub     ed25519.PublicKey
	http    *http.Client
	goos    string
	goarch  string
	kinds   []string
	log     *slog.Logger
}

// NewClient builds a Client. It fails fast when no usable public key is
// configured: an updater that cannot verify must never update.
func NewClient(opts Options) (*Client, error) {
	c := &Client{
		feedURL: opts.FeedURL,
		channel: opts.Channel,
		current: opts.CurrentVersion,
		http:    opts.HTTPClient,
		goos:    opts.GOOS,
		goarch:  opts.GOARCH,
		kinds:   opts.PreferKinds,
		log:     opts.Logger,
	}
	if c.feedURL == "" {
		c.feedURL = DefaultFeedBase
	}
	if c.channel == "" {
		c.channel = ChannelStable
	}
	if c.channel != ChannelStable && c.channel != ChannelBeta {
		return nil, fmt.Errorf("updater: channel %q (want stable|beta)", c.channel)
	}
	if c.current == "" {
		c.current = Version
	}
	if c.http == nil {
		c.http = egress.Client(egress.CapDirect, 30*time.Second)
	}
	if c.goos == "" {
		c.goos = runtime.GOOS
	}
	if c.goarch == "" {
		c.goarch = runtime.GOARCH
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	pub := opts.PublicKey
	if pub == nil {
		var err error
		pub, err = DefaultPublicKey()
		if err != nil {
			return nil, err
		}
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, ErrNoPublicKey
	}
	c.pub = pub
	return c, nil
}

// NewClientFromConfig maps the owner's Updates settings onto a Client.
func NewClientFromConfig(upd config.Updates, logger *slog.Logger) (*Client, error) {
	return NewClient(Options{
		FeedURL: upd.FeedURL,
		Channel: upd.Channel,
		Logger:  logger,
	})
}

// CurrentVersion is the version Check compares against.
func (c *Client) CurrentVersion() string { return c.current }

// ManifestURL resolves the feed URL for the configured channel:
//   - a URL containing "{channel}" has it substituted;
//   - a URL ending in ".json" is used as-is;
//   - anything else is a base: "<base>/<channel>.json".
func (c *Client) ManifestURL() string { return manifestURL(c.feedURL, c.channel) }

func manifestURL(feed, channel string) string {
	if strings.Contains(feed, "{channel}") {
		return strings.ReplaceAll(feed, "{channel}", channel)
	}
	if strings.HasSuffix(feed, ".json") {
		return feed
	}
	return strings.TrimSuffix(feed, "/") + "/" + channel + ".json"
}

// Update is a verified, newer release with the artifact for this platform.
type Update struct {
	Manifest *Manifest
	Artifact *Artifact
}

// Version is the update's version.
func (u *Update) Version() string { return u.Manifest.Version }

// Check fetches and verifies the channel manifest. It returns (nil, nil)
// when the feed's version is not newer than the running one.
func (c *Client) Check(ctx context.Context) (*Update, error) {
	m, err := c.channelManifest(ctx)
	if err != nil {
		return nil, err
	}
	if m.MinVersion != "" {
		if cmp, err := CompareVersions(c.current, m.MinVersion); err == nil && cmp < 0 {
			return nil, fmt.Errorf("%w (installed %s, needs >= %s)", ErrTooOldToUpdate, c.current, m.MinVersion)
		}
	}
	if !Newer(m.Version, c.current) {
		c.log.Debug("updater: no update", "current", c.current, "feed", m.Version)
		return nil, nil
	}
	art, err := m.ArtifactFor(c.goos, c.goarch, c.kinds...)
	if err != nil {
		return nil, err
	}
	c.log.Info("updater: update available", "current", c.current, "available", m.Version, "kind", art.Kind)
	return &Update{Manifest: m, Artifact: art}, nil
}

// channelManifest is the manifest to offer: the channel's own, and for the
// beta channel the stable manifest too when it is newer (a stable release
// following a beta must reach beta installs as well). With a single
// manifest URL configured only that manifest is consulted.
func (c *Client) channelManifest(ctx context.Context) (*Manifest, error) {
	m, err := c.fetchChannel(ctx, c.channel)
	stableURL := manifestURL(c.feedURL, "stable")
	if c.channel != "beta" || stableURL == c.ManifestURL() {
		return m, err
	}
	stable, serr := c.fetchChannel(ctx, "stable")
	switch {
	case err != nil && serr != nil:
		return nil, err
	case err != nil:
		return stable, nil
	case serr == nil && Newer(stable.Version, m.Version):
		return stable, nil
	}
	return m, nil
}

// fetchChannel fetches and verifies channel's manifest.
func (c *Client) fetchChannel(ctx context.Context, channel string) (*Manifest, error) {
	m, err := c.fetchManifest(ctx, manifestURL(c.feedURL, channel))
	if err != nil {
		return nil, err
	}
	if m.Channel != channel {
		return nil, fmt.Errorf("updater: feed channel %q, want %q", m.Channel, channel)
	}
	return m, nil
}

func (c *Client) fetchManifest(ctx context.Context, url string) (*Manifest, error) {
	body, err := c.get(ctx, url, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("updater: fetch manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("updater: parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.Signature != "" {
		if err := m.Verify(c.pub); err != nil {
			return nil, err
		}
		if err := m.ValidateFreshness(time.Now()); err != nil {
			return nil, err
		}
		return &m, nil
	}
	// Detached signature: "<manifest>.sig", base64 over the exact bytes.
	sigBody, err := c.get(ctx, url+".sig", 4096)
	if err != nil {
		return nil, fmt.Errorf("%w (no embedded signature and detached fetch failed: %v)", ErrBadSignature, err)
	}
	if err := VerifyDetached(c.pub, body, string(sigBody)); err != nil {
		return nil, err
	}
	if err := m.ValidateFreshness(time.Now()); err != nil {
		return nil, err
	}
	return &m, nil
}

func (c *Client) get(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Ether/"+c.current+" updater")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("GET %s: response over %d bytes", url, maxBytes)
	}
	return b, nil
}

// ShouldAutoApply interprets the owner's update policy: auto-apply only
// when config says so; the tray/server otherwise prompts.
func ShouldAutoApply(upd config.Updates) bool { return upd.AutoUpdate }

// Poll periodically checks the feed until ctx ends, invoking onUpdate for
// each newly-seen update version. Check errors are logged and retried on
// the next tick; the first check runs immediately unless firstDelay > 0.
func (c *Client) Poll(ctx context.Context, interval, firstDelay time.Duration, onUpdate func(context.Context, *Update)) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	seen := ""
	check := func() {
		upd, err := c.Check(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("updater: check failed", "err", err)
			}
			return
		}
		if upd != nil && upd.Version() != seen {
			seen = upd.Version()
			if onUpdate != nil {
				onUpdate(ctx, upd)
			}
		}
	}
	if !schedule.Wait(ctx, time.Now, firstDelay) {
		return
	}
	for ctx.Err() == nil {
		check()
		if !schedule.Wait(ctx, time.Now, interval) {
			return
		}
	}
}

func parseHexSHA(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("updater: sha256 must be 32 hex-encoded bytes")
	}
	return b, nil
}

// UserAgent is exposed for tests/logging of download requests.
func (c *Client) userAgent() string { return "Ether/" + c.current + " updater" }

// tempDir returns a writable scratch dir for downloads.
func tempDir() string {
	if d := os.Getenv("ETHER_UPDATE_DIR"); d != "" {
		return d
	}
	return os.TempDir()
}

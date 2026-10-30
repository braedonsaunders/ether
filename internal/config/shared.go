package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"
)

// Cluster mode.
//
// A standalone server keeps every setting in config.json. A cluster node
// boots in two steps: Open reads config.json (which names the database),
// the database is opened, then AttachShared overlays the shared settings
// (see scope.go) from the database. From then on Update writes the shared
// part to the database with an optimistic version check and the whole
// config to config.json, where the shared part is only a mirror that is
// replaced from the database at every start. Changes made on other nodes
// arrive through the Notifier (topic SharedTopic) or, as a fallback, by
// polling the version.

const (
	// SharedTopic is published (payload: the new version, decimal) after a
	// node changes the shared settings.
	SharedTopic = "config:changed"
	// ResyncTopic is the event bus's synthetic "you may have missed
	// events" notice after a reconnect; the store re-reads its version.
	ResyncTopic = "bus:resync"
	// DefaultSharedPoll is the fallback poll interval.
	DefaultSharedPoll = 30 * time.Second
)

// Notifier is the cluster event bus as the config store uses it. A nil
// Notifier means no cross-node push; polling still picks changes up.
type Notifier interface {
	Publish(ctx context.Context, topic string, payload []byte) error
	Subscribe(topic string, fn func([]byte)) (unsubscribe func())
}

// SharedDoc is the stored form of the shared settings.
type SharedDoc struct {
	// Doc is the shared settings JSON with secrets sealed by the cluster's
	// credential key.
	Doc string
	// Version increases by one on every write.
	Version int64
	// KeyFingerprint is KeyFingerprint(credential key) of the cluster.
	KeyFingerprint string
}

var (
	// ErrSharedConflict is returned by SharedBackend.SaveShared when the
	// stored version is not the expected one.
	ErrSharedConflict = errors.New("config: shared settings were changed by another node")
	// ErrKeyMismatch means this node's credential key is not the cluster's.
	ErrKeyMismatch = errors.New("config: this node's credential key does not match the cluster's")
	// ErrSharedMissing means the database holds no shared settings yet.
	ErrSharedMissing = errors.New("config: the cluster database holds no shared settings")
)

// SharedBackend stores the shared settings document (implemented over the
// database by internal/clusterconfig).
type SharedBackend interface {
	// LoadShared returns the stored document; found is false when none
	// has been stored yet.
	LoadShared(ctx context.Context) (doc SharedDoc, found bool, err error)
	// SaveShared stores doc if the stored version is expectVersion (0:
	// nothing stored yet) and returns the new version, or
	// ErrSharedConflict.
	SaveShared(ctx context.Context, doc SharedDoc, expectVersion int64) (int64, error)
	// SharedVersion is the stored version (0 when none).
	SharedVersion(ctx context.Context) (int64, error)
}

// SharedOptions configure AttachShared.
type SharedOptions struct {
	// Notifier pushes changes between nodes; may also be set later with
	// SetNotifier. Nil: polling only.
	Notifier Notifier
	// PollInterval re-reads the stored version (default
	// DefaultSharedPoll; negative disables polling and the watcher).
	PollInterval time.Duration
	// Timeout bounds each database call (default 10s).
	Timeout time.Duration
	// NoSeed makes AttachShared fail with ErrSharedMissing instead of
	// seeding an empty database from this node's settings.
	NoSeed bool
	Logger *slog.Logger
}

// maxSharedAttempts bounds Update's retries on version conflicts.
const maxSharedAttempts = 8

// Clustered reports whether shared settings come from the database.
func (s *Store) Clustered() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shared != nil
}

// SharedVersion is the version of the shared settings this node holds
// (0 on a standalone server).
func (s *Store) SharedVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sharedVer
}

// KeyFingerprint identifies this node's credential key.
func (s *Store) KeyFingerprint() string { return KeyFingerprint(s.credentialKey) }

// CredentialKey returns a copy of this node's credential key. Only the
// cluster join-token issuer needs it; never log or store it.
func (s *Store) CredentialKey() []byte { return append([]byte(nil), s.credentialKey...) }

// Seal encrypts plain with the credential key, bound to name.
func (s *Store) Seal(name string, plain []byte) (string, error) {
	return sealBytes(s.credentialKey, name, plain)
}

// Unseal reverses Seal for the same name.
func (s *Store) Unseal(name, sealed string) ([]byte, error) {
	return openBytes(s.credentialKey, name, sealed)
}

func (o SharedOptions) withDefaults() SharedOptions {
	if o.PollInterval == 0 {
		o.PollInterval = DefaultSharedPoll
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o
}

// AttachShared switches the store to cluster mode: it loads the shared
// settings from b (seeding b from this node's settings when it holds none,
// unless NoSeed) and overlays them on the node-local ones, refusing with
// ErrKeyMismatch when the cluster was sealed with another credential key.
// Until ctx ends it then follows changes made by other nodes.
func (s *Store) AttachShared(ctx context.Context, b SharedBackend, o SharedOptions) error {
	return s.policyUpdate(func() error { return s.attachShared(ctx, b, o) })
}

func (s *Store) attachShared(ctx context.Context, b SharedBackend, o SharedOptions) error {
	if b == nil {
		return errors.New("config: no shared settings backend")
	}
	o = o.withDefaults()
	s.writeMu.Lock()
	if s.Clustered() {
		s.writeMu.Unlock()
		return errors.New("config: shared settings already attached")
	}
	before := s.Get()
	lctx, cancel := context.WithTimeout(ctx, o.Timeout)
	doc, found, err := b.LoadShared(lctx)
	if err == nil && !found {
		if o.NoSeed {
			err = ErrSharedMissing
		} else {
			var sealed string
			if sealed, err = s.sealShared(SharedJSON(before)); err == nil {
				var v int64
				v, err = b.SaveShared(lctx, SharedDoc{Doc: sealed, KeyFingerprint: s.KeyFingerprint()}, 0)
				if err == nil {
					doc, found = SharedDoc{Doc: sealed, Version: v, KeyFingerprint: s.KeyFingerprint()}, true
					o.Logger.Info("config: seeded the cluster's shared settings from this node", "version", v)
				} else if errors.Is(err, ErrSharedConflict) {
					// Another node seeded first; adopt its settings.
					doc, found, err = b.LoadShared(lctx)
					if err == nil && !found {
						err = ErrSharedMissing
					}
				}
			}
		}
	}
	cancel()
	var next Config
	if err == nil {
		next, err = s.mergeDoc(before, doc)
	}
	if err != nil {
		s.writeMu.Unlock()
		return err
	}
	s.mu.Lock()
	s.cfg, s.sharedVer = next, doc.Version
	s.shared, s.sharedOpts = b, o
	saveErr := s.save()
	subs := append([]func(Config){}, s.subs...)
	s.mu.Unlock()
	s.writeMu.Unlock()
	if saveErr != nil {
		o.Logger.Warn("config: write local mirror", "err", saveErr)
	}
	if o.Notifier != nil {
		s.SetNotifier(o.Notifier)
	}
	if o.PollInterval > 0 {
		wctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		s.mu.Lock()
		s.stopWatch = func() { cancel(); <-done }
		s.mu.Unlock()
		go func() {
			defer close(done)
			s.watchShared(wctx, o.PollInterval)
		}()
	}
	if !sameConfig(before, next) {
		for _, f := range subs {
			f(clone(next))
		}
	}
	return nil
}

// Close stops following other nodes' changes (the watcher started by
// AttachShared and the notifier subscriptions) and waits for an in-flight
// refresh to finish. The store keeps working; it no longer refreshes.
func (s *Store) Close() {
	s.SetNotifier(nil)
	s.mu.Lock()
	stop := s.stopWatch
	s.stopWatch = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// SetNotifier connects (or, with nil, disconnects) the cluster event bus.
// It may be called before or after AttachShared.
func (s *Store) SetNotifier(n Notifier) {
	s.mu.Lock()
	old := s.unsubs
	s.unsubs, s.notifier = nil, n
	s.mu.Unlock()
	for _, u := range old {
		if u != nil {
			u()
		}
	}
	if n == nil {
		return
	}
	unsubs := []func(){n.Subscribe(SharedTopic, s.onNotify), n.Subscribe(ResyncTopic, s.onNotify)}
	s.mu.Lock()
	if s.notifier == n {
		s.unsubs = unsubs
		unsubs = nil
	}
	s.mu.Unlock()
	for _, u := range unsubs { // superseded by a concurrent SetNotifier
		if u != nil {
			u()
		}
	}
}

func (s *Store) onNotify(payload []byte) {
	if v, err := strconv.ParseInt(string(bytes.TrimSpace(payload)), 10, 64); err == nil && v <= s.SharedVersion() {
		return // our own write, or already applied
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Store) watchShared(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
		s.mu.RLock()
		o := s.sharedOpts
		s.mu.RUnlock()
		rctx, cancel := context.WithTimeout(ctx, o.Timeout)
		if err := s.RefreshShared(rctx); err != nil && ctx.Err() == nil {
			o.Logger.Warn("config: refresh shared settings", "err", err)
		}
		cancel()
	}
}

// RefreshShared applies the stored shared settings if another node changed
// them, notifying subscribers. It is a no-op on a standalone server.
func (s *Store) RefreshShared(ctx context.Context) error {
	return s.policyUpdate(func() error { return s.refreshShared(ctx) })
}

func (s *Store) refreshShared(ctx context.Context) error {
	s.writeMu.Lock()
	s.mu.RLock()
	b, have := s.shared, s.sharedVer
	s.mu.RUnlock()
	if b == nil {
		s.writeMu.Unlock()
		return nil
	}
	v, err := b.SharedVersion(ctx)
	if err != nil || v == have {
		s.writeMu.Unlock()
		return err
	}
	doc, found, err := b.LoadShared(ctx)
	if err == nil && !found {
		err = ErrSharedMissing
	}
	if err != nil {
		s.writeMu.Unlock()
		return err
	}
	before := s.Get()
	next, err := s.mergeDoc(before, doc)
	if err != nil {
		s.writeMu.Unlock()
		return err
	}
	s.mu.Lock()
	s.cfg, s.sharedVer = next, doc.Version
	changed := !sameConfig(before, next)
	var saveErr error
	if changed {
		saveErr = s.save()
	}
	subs := append([]func(Config){}, s.subs...)
	s.mu.Unlock()
	s.writeMu.Unlock()
	if changed {
		for _, f := range subs {
			f(clone(next))
		}
	}
	if saveErr != nil {
		return fmt.Errorf("config: write local mirror: %w", saveErr)
	}
	return nil
}

func (s *Store) updateClustered(fn func(*Config) error) error {
	s.writeMu.Lock()
	s.mu.RLock()
	b, ver, o := s.shared, s.sharedVer, s.sharedOpts
	s.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	start := s.Get()
	base := start
	var next Config
	published := false
	for attempt := 1; ; attempt++ {
		next = clone(base)
		if err := fn(&next); err != nil {
			s.writeMu.Unlock()
			return err
		}
		newShared := SharedJSON(next)
		if bytes.Equal(newShared, SharedJSON(base)) {
			break
		}
		sealed, err := s.sealShared(newShared)
		if err != nil {
			s.writeMu.Unlock()
			return err
		}
		v, err := b.SaveShared(ctx, SharedDoc{Doc: sealed, KeyFingerprint: s.KeyFingerprint()}, ver)
		if err == nil {
			ver, published = v, true
			break
		}
		if !errors.Is(err, ErrSharedConflict) || attempt >= maxSharedAttempts {
			s.writeMu.Unlock()
			return fmt.Errorf("config: save shared settings: %w", err)
		}
		// Another node wrote first: rebase on its settings and re-apply fn.
		doc, found, lerr := b.LoadShared(ctx)
		if lerr == nil && !found {
			lerr = ErrSharedMissing
		}
		if lerr == nil {
			base, lerr = s.mergeDoc(base, doc)
		}
		if lerr != nil {
			s.writeMu.Unlock()
			return fmt.Errorf("config: reload shared settings: %w", lerr)
		}
		ver = doc.Version
	}
	s.mu.Lock()
	s.cfg, s.sharedVer = next, ver
	saveErr := s.save()
	subs := append([]func(Config){}, s.subs...)
	n := s.notifier
	s.mu.Unlock()
	s.writeMu.Unlock()
	if published && n != nil {
		if err := n.Publish(ctx, SharedTopic, []byte(strconv.FormatInt(ver, 10))); err != nil {
			o.Logger.Warn("config: announce shared settings change", "err", err)
		}
	}
	// The shared part is committed; a failed local write is reported but
	// the running settings follow the database.
	if !sameConfig(start, next) {
		for _, f := range subs {
			f(clone(next))
		}
	}
	return saveErr
}

func (s *Store) sealShared(plain []byte) (string, error) {
	b, err := sealDocument(plain, s.credentialKey, s.bound.Load())
	return string(b), err
}

// mergeDoc overlays a stored shared document on base's node-local part.
func (s *Store) mergeDoc(base Config, doc SharedDoc) (Config, error) {
	if fp := s.KeyFingerprint(); doc.KeyFingerprint != fp {
		return Config{}, fmt.Errorf("%w (cluster %.12s, this node %.12s); add this node with `ether cluster join <token>`", ErrKeyMismatch, doc.KeyFingerprint, fp)
	}
	plain, stats, err := openDocument([]byte(doc.Doc), s.credentialKey)
	if err != nil {
		return Config{}, fmt.Errorf("config: open shared settings: %w", err)
	}
	if stats.strict {
		// Another node upgraded the cluster's credentials, so every live
		// node already reads v2: seal database fields and shared settings
		// now rather than at this node's next upgrade pass. config.json
		// waits for UpgradeCredentials (this node's rollback gate).
		s.bound.Store(true)
	}
	return MergeShared(base, plain)
}

func sameConfig(a, b Config) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

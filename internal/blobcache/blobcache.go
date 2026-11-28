// Package blobcache is Ether's on-disk whole-blob cache for plugin content
// kinds (spec §5.4, §15.5). Each library owns an isolated namespace with its
// own directory, persistent index, quota, and optional TTL, while all
// production file reservations share the playback cache budget and disk floor.
package blobcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/atomicfile"
	"github.com/braedonsaunders/ether/internal/segcache"
)

// Default budget constants (spec §15.5).
const (
	DefaultMaxBytes   int64 = 2 << 30  // 2 GiB per library default
	DefaultSharedCap  int64 = 10 << 30 // 10 GiB shared cap across libraries
	sharedDiskPercent int64 = 20       // 5% of total disk (total / 20)
	defaultSweepEvery       = 15 * time.Minute
	atimeSaveDebounce       = 30 * time.Second
)

var (
	// ErrNotFound is returned when a blob reference or key is not in the cache.
	ErrNotFound = errors.New("blobcache: blob not found")
	// ErrTooLarge is returned when a blob exceeds the namespace or shared quota.
	ErrTooLarge = errors.New("blobcache: blob exceeds quota")
	// ErrQuotaFull is returned when a write or reservation cannot fit because
	// the namespace quota or shared cap is currently occupied by non-evictable
	// data (such as open blobs or active partials) or tight disk headroom.
	// Errors matching ErrQuotaFull also match ErrTooLarge via errors.Is.
	ErrQuotaFull = errors.New("blobcache: quota reached")
	// ErrClosed is returned on mutating operations after Close.
	ErrClosed = errors.New("blobcache: cache closed")
	// ErrBadRef is returned when a blob reference is not "blob:<64-hex>".
	ErrBadRef = errors.New("blobcache: invalid blob reference")
)

type quotaFullError struct {
	msg string
}

func (e quotaFullError) Error() string { return e.msg }

func (e quotaFullError) Is(target error) bool {
	return target == ErrTooLarge || target == ErrQuotaFull
}

func quotaFullErrorf(format string, args ...any) error {
	return quotaFullError{
		msg: fmt.Sprintf("%s: %s", ErrTooLarge.Error(), fmt.Sprintf(format, args...)),
	}
}

// Ref is a content-addressed blob reference ("blob:<sha256>").
type Ref string

// String returns the canonical "blob:<sha256>" form.
func (r Ref) String() string { return string(r) }

// Hash returns the 64-character lowercase hex SHA-256 digest of r, or an error
// if r is not a valid blob reference.
func (r Ref) Hash() (string, error) {
	return ParseRef(string(r))
}

// ParseRef validates a "blob:<sha256>" string and returns its lowercase 64-char
// hex digest.
func ParseRef(raw string) (string, error) {
	hexPart, ok := strings.CutPrefix(strings.TrimSpace(raw), "blob:")
	if !ok || len(hexPart) != 64 {
		return "", ErrBadRef
	}
	b, err := hex.DecodeString(hexPart)
	if err != nil || len(b) != sha256.Size {
		return "", ErrBadRef
	}
	return strings.ToLower(hexPart), nil
}

// Quota configures a library namespace's maximum bytes and optional TTL.
// A zero MaxBytes uses DefaultMaxBytes (2 GiB).
type Quota struct {
	MaxBytes int64
	TTL      time.Duration
}

// EffectiveMaxBytes returns the library ceiling. A zero quota inherits the pool.
func (q Quota) EffectiveMaxBytes() int64 {
	if q.MaxBytes <= 0 {
		return 1<<63 - 2
	}
	return q.MaxBytes
}

// ByteSource is a random-access, closable byte stream with size, name and MIME
// metadata. It matches kindstream.ByteSource and also implements io.ReadSeekCloser.
type ByteSource interface {
	io.ReaderAt
	io.ReadSeekCloser
	Size() int64
	Name() string
	MIME() string
}

// Options configures a Cache.
type Options struct {
	CheckVolume  func() error
	Unified      bool
	Shared       bool
	PoolCapacity func() int64
	MakeFileRoom func(accounted, unwritten, additional int64, candidates segcache.FileCandidates) error

	Logger              *slog.Logger
	DiskUsage           segcache.DiskUsageFunc
	ConfiguredSharedCap func() int64  // Positive overrides the automatic size and percentage limits.
	SharedCap           int64         // 0 = DefaultSharedCap (10 GiB)
	SweepPeriod         time.Duration // 0 = 15m; negative disables background ticker
	Now                 func() time.Time
}

// Cache manages per-library blob namespaces under a root directory.
type Cache struct {
	poolMu  sync.Mutex
	options Options

	dir                 string
	logger              *slog.Logger
	sharedCap           int64
	configuredSharedCap func() int64
	now                 func() time.Time

	floorMu sync.Mutex
	diskFn  segcache.DiskUsageFunc

	evictMu sync.Mutex // serializes global cap eviction and cross-namespace reservations

	mu         sync.Mutex
	closed     bool
	namespaces map[string]*Namespace
	pluginLibs map[string]map[string]bool // pluginID -> set of libraryKeys

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// Open opens or creates a blob cache rooted at dir.
func Open(dir string, opts ...Options) (*Cache, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("blobcache: empty root directory")
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Shared {
		o.Unified = true
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	sharedCap := o.SharedCap
	if sharedCap <= 0 {
		sharedCap = DefaultSharedCap
	}
	if o.CheckVolume != nil {
		if err := o.CheckVolume(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("blobcache: mkdir %s: %w", dir, err)
	}
	c := &Cache{
		options: o, dir: dir,
		logger:              o.Logger.With("component", "blobcache"),
		sharedCap:           sharedCap,
		configuredSharedCap: o.ConfiguredSharedCap,
		now:                 o.Now,
		diskFn:              o.DiskUsage,
		namespaces:          make(map[string]*Namespace),
		pluginLibs:          make(map[string]map[string]bool),
		stopCh:              make(chan struct{}),
		doneCh:              make(chan struct{}),
	}
	if o.Shared {
		for _, name := range []string{".allocation.lock", ".staging.lock"} {
			f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return nil, err
			}
			f.Close()
		}
	}
	if o.Unified {
		unlock, err := c.lockPool(context.Background())
		if err != nil {
			return nil, err
		}
		defer unlock()
		if o.Shared {
			if err := os.Remove(filepath.Join(dir, ".idle-usage")); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
	}
	// Load any existing namespaces on disk so shared usage accounting is complete.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("blobcache: readdir %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		ns, err := c.openNamespaceLocked(e.Name(), Quota{})
		if err != nil {
			if o.Unified {
				return nil, err
			}
			c.logger.Warn("blobcache: skipping unreadable namespace", "library", e.Name(), "err", err)
			continue
		}
		if o.Unified {
			if err := ns.recoverPublication(); err != nil {
				return nil, err
			}
		}
		c.namespaces[e.Name()] = ns
	}
	period := o.SweepPeriod
	if period == 0 {
		period = defaultSweepEvery
	}
	if period > 0 {
		go c.sweepLoop(period)
	} else {
		close(c.doneCh)
	}
	return c, nil
}

func (c *Cache) sweepLoop(period time.Duration) {
	defer close(c.doneCh)
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			c.SweepAll()
		}
	}
}

// Close stops background sweeping and flushes namespace indexes.
func (c *Cache) Close() error {
	c.mu.Lock()
	c.closed = true
	nss := make([]*Namespace, 0, len(c.namespaces))
	for _, ns := range c.namespaces {
		nss = append(nss, ns)
	}
	c.mu.Unlock()

	c.stopOnce.Do(func() {
		close(c.stopCh)
		<-c.doneCh
	})
	for _, ns := range nss {
		if c.options.Shared {
			continue
		}
		ns.flushIndex()
	}
	return nil
}

// SetDiskUsage sets or updates the volume free-space measurement callback.
func (c *Cache) SetDiskUsage(fn segcache.DiskUsageFunc) {
	c.floorMu.Lock()
	c.diskFn = fn
	c.floorMu.Unlock()
	c.SweepAll()
}

// BindPluginLibrary associates libraryKey with pluginID so HostKindBackends
// CacheOpen can scope blob lookups to the calling plugin's own libraries.
func (c *Cache) BindPluginLibrary(pluginID, libraryKey string) {
	if pluginID == "" || libraryKey == "" {
		return
	}
	c.mu.Lock()
	set := c.pluginLibs[pluginID]
	if set == nil {
		set = make(map[string]bool)
		c.pluginLibs[pluginID] = set
	}
	first := !set[libraryKey]
	set[libraryKey] = true
	c.mu.Unlock()
	if first && c.options.Shared {
		c.Namespace(libraryKey, Quota{})
	}
}

// Namespace returns (creating if needed) the per-library blob namespace for
// libraryKey and applies quota.
func (c *Cache) Namespace(libraryKey string, quota Quota) *Namespace {
	key := sanitizeNamespaceKey(libraryKey)
	trimmed := strings.TrimSpace(libraryKey)
	c.mu.Lock()
	ns, ok := c.namespaces[key]
	if !ok {
		var err error
		ns, err = c.openNamespaceLocked(key, quota)
		if err != nil {
			c.logger.Warn("blobcache: create namespace failed", "library", key, "err", err)
			ns = newEmptyNamespace(c, key, quota)
		}
		if trimmed != "" {
			ns.libraryKey = trimmed
		}
		c.namespaces[key] = ns
	} else {
		if trimmed != "" {
			ns.mu.Lock()
			if ns.libraryKey == "" || ns.libraryKey == ns.key {
				ns.libraryKey = trimmed
			}
			ns.mu.Unlock()
		}
		if quota.MaxBytes > 0 || quota.TTL > 0 {
			ns.mu.Lock()
			ns.declared = quota
			ns.applyQuotaLocked()
			ns.mu.Unlock()
		}
	}
	c.mu.Unlock()
	return ns
}

// SweepAll sweeps every namespace and enforces the shared cap and disk floor.
func (c *Cache) SweepAll() {
	unlock, err := c.guard(context.Background())
	if err != nil {
		return
	}
	defer unlock()
	c.mu.Lock()
	nss := make([]*Namespace, 0, len(c.namespaces))
	for _, ns := range c.namespaces {
		nss = append(nss, ns)
	}
	c.mu.Unlock()
	for _, ns := range nss {
		ns.sweep(0, "")
	}
	c.evictMu.Lock()
	defer c.evictMu.Unlock()
	c.enforceGlobalLimitLocked(0, nil, "")
}

// TotalStats returns aggregate bytes and blob count across all namespaces.
func (c *Cache) TotalStats() (bytes int64, blobs int) {
	if c.options.Shared {
		unlock, err := c.readGuard()
		if err != nil {
			return 0, 0
		}
		defer unlock()
	}
	for _, ns := range c.listNamespaces() {
		b, n := ns.storedStats()
		bytes += b
		blobs += n
	}
	return bytes, blobs
}

func (c *Cache) totalAccounted() int64 {
	accounted, _ := c.usage()
	return accounted
}

// EffectiveSharedCap computes the global cap across all plugin libraries,
// applying the free-space reserve to both automatic and owner-configured caps.
func (c *Cache) EffectiveSharedCap() int64 {
	if c.options.Unified {
		if c.options.PoolCapacity != nil {
			return c.options.PoolCapacity()
		}
		return 1<<63 - 2
	}
	capBytes := c.sharedCap
	var configured int64
	if c.configuredSharedCap != nil {
		configured = c.configuredSharedCap()
	}
	if configured > 0 {
		capBytes = configured
	}
	c.floorMu.Lock()
	diskFn := c.diskFn
	c.floorMu.Unlock()
	if diskFn == nil {
		return capBytes
	}
	used, _ := c.TotalStats()
	total, free, err := diskFn()
	if err != nil || total <= 0 {
		return capBytes
	}
	fivePct := total / sharedDiskPercent
	if configured <= 0 && fivePct < capBytes {
		capBytes = fivePct
	}
	floor := segcache.MinFreeBytes(total)
	avail := max(used+free-floor, 0)
	if avail < capBytes {
		capBytes = avail
	}
	return capBytes
}

// globalCandidate pairs a namespace with one of its evictable entries.
type globalCandidate struct {
	ns       *Namespace
	hash     string
	lastOpen int64
	size     int64
}

// enforceGlobalLimit evicts idle partials and least-recently-opened non-open
// blobs across all namespaces until totalAccounted() + need <= EffectiveSharedCap().
// Active partials and currently open blobs are never evicted.
func (c *Cache) enforceGlobalLimit(need int64) {
	c.enforceGlobalLimitProtected(need, nil, "")
}

func (c *Cache) enforceGlobalLimitProtected(need int64, protectNS *Namespace, protectHash string) {
	c.evictMu.Lock()
	defer c.evictMu.Unlock()
	c.enforceGlobalLimitLocked(need, protectNS, protectHash)
}

func (c *Cache) enforceGlobalLimitLocked(need int64, protectNS *Namespace, protectHash string) {
	if need < 0 {
		need = 0
	}
	limit := c.EffectiveSharedCap()
	target := limit
	if need <= limit {
		target = limit - need
	}
	accounted := c.totalAccounted()
	if accounted <= target {
		return
	}
	c.mu.Lock()
	nss := make([]*Namespace, 0, len(c.namespaces))
	for _, ns := range c.namespaces {
		nss = append(nss, ns)
	}
	c.mu.Unlock()

	// Evict idle incomplete partials oldest-first before touching completed blobs.
	for _, pc := range c.collectIdlePartials(nss) {
		if accounted <= target {
			break
		}
		if freed := pc.ns.evictPartial(pc.id); freed > 0 {
			accounted -= freed
		}
	}
	if accounted <= target {
		return
	}

	var cands []globalCandidate
	for _, ns := range nss {
		ns.mu.Lock()
		for h, e := range ns.blobs {
			if ns.isOpenLocked(h) || (ns == protectNS && h == protectHash) {
				continue
			}
			cands = append(cands, globalCandidate{
				ns:       ns,
				hash:     h,
				lastOpen: e.LastOpen,
				size:     e.Size,
			})
		}
		ns.mu.Unlock()
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].lastOpen != cands[j].lastOpen {
			return cands[i].lastOpen < cands[j].lastOpen
		}
		return cands[i].hash < cands[j].hash
	})
	touched := make(map[*Namespace]bool)
	for _, cand := range cands {
		if accounted <= target {
			break
		}
		if freed := cand.ns.evictHash(cand.hash); freed > 0 {
			accounted -= freed
			touched[cand.ns] = true
		}
	}
	for ns := range touched {
		ns.flushIndex()
	}
}

// HostBackends returns CachePut, CacheOpen, and CacheDelete functions suitable
// for wiring into plugins.KindBackends (spec §3.6, §5.4).
//
// quotaFor may be nil, in which case the namespace's existing or default quota
// is used. CacheOpen and CacheDelete search only namespaces explicitly bound
// to pluginID via BindPluginLibrary.
func (c *Cache) HostBackends(quotaFor func(pluginID, libraryKey string) Quota) (
	cachePut func(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (string, error),
	cacheOpen func(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error),
	cacheDelete func(ctx context.Context, pluginID, blobRef string) error,
) {
	cachePut = func(ctx context.Context, pluginID, libraryKey, name string, r io.Reader) (string, error) {
		c.BindPluginLibrary(pluginID, libraryKey)
		var q Quota
		if quotaFor != nil {
			q = quotaFor(pluginID, libraryKey)
		}
		ns := c.Namespace(libraryKey, q)
		size := int64(-1)
		if known, ok := r.(interface{ ExpectedSize() int64 }); ok {
			size = known.ExpectedSize()
		}
		ref, err := ns.Put(ctx, name, r, size)
		if err != nil {
			return "", err
		}
		return ref.String(), nil
	}
	cacheOpen = func(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := ParseRef(blobRef); err != nil {
			return nil, err
		}
		nss, err := c.namespacesForPlugin(pluginID)
		if err != nil {
			return nil, err
		}
		for _, ns := range nss {
			src, err := ns.Open(Ref(blobRef))
			if err == nil {
				return src, nil
			}
			if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
		return nil, ErrNotFound
	}
	cacheDelete = func(ctx context.Context, pluginID, blobRef string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := ParseRef(blobRef); err != nil {
			return err
		}
		if strings.TrimSpace(pluginID) == "" {
			return nil
		}
		nss, err := c.namespacesForPlugin(pluginID)
		if err != nil {
			return err
		}
		for _, ns := range nss {
			if _, err := ns.Delete(Ref(blobRef)); err != nil {
				return err
			}
		}
		return nil
	}
	return cachePut, cacheOpen, cacheDelete
}

// OpenPluginBlobForStat opens an owned blob without synchronously updating
// the on-disk index. Explicit bindings are populated when libraries are reconciled.
func (c *Cache) OpenPluginBlobForStat(ctx context.Context, pluginID, blobRef string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	owned := c.pluginLibs[pluginID]
	namespaces := make([]*Namespace, 0, len(owned))
	for key := range owned {
		if ns := c.namespaces[sanitizeNamespaceKey(key)]; ns != nil {
			namespaces = append(namespaces, ns)
		}
	}
	c.mu.Unlock()
	for _, ns := range namespaces {
		src, err := ns.open(Ref(blobRef), false)
		if !errors.Is(err, ErrNotFound) {
			return src, err
		}
	}
	return nil, ErrNotFound
}

// NamespaceForPluginBlob returns the plugin-owned Namespace that holds blobRef,
// or nil when no namespace owned by pluginID contains it.
func (c *Cache) NamespaceForPluginBlob(pluginID, blobRef string) *Namespace {
	if c == nil || strings.TrimSpace(pluginID) == "" {
		return nil
	}
	if _, err := ParseRef(blobRef); err != nil {
		return nil
	}
	nss, err := c.namespacesForPlugin(pluginID)
	if err != nil {
		return nil
	}
	for _, ns := range nss {
		if src, err := ns.Open(Ref(blobRef)); err == nil {
			_ = src.Close()
			return ns
		}
	}
	return nil
}

func (c *Cache) namespacesForPlugin(pluginID string) ([]*Namespace, error) {
	if c.options.Shared {
		c.mu.Lock()
		bound := len(c.pluginLibs[pluginID]) > 0
		c.mu.Unlock()
		if !bound {
			// A peer can publish the first file in a canonical namespace before
			// this node has reconciled the matching plugin's libraries.
			unlock, err := c.readGuard()
			if err != nil {
				return nil, err
			}
			unlock()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*Namespace
	seen := make(map[*Namespace]bool)
	if set := c.pluginLibs[pluginID]; len(set) > 0 {
		for libKey := range set {
			if ns := c.namespaces[sanitizeNamespaceKey(libKey)]; ns != nil && !seen[ns] {
				seen[ns] = true
				out = append(out, ns)
			}
		}
	}
	return out, nil
}

func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	return b.String()
}

func sanitizeNamespaceKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	res := strings.Trim(b.String(), ".")
	if res == "" {
		return "default"
	}
	return res
}

// ---------------------------------------------------------------------------
// Namespace

type blobEntry struct {
	Hash      string   `json:"hash"`
	Size      int64    `json:"size"`
	Name      string   `json:"name,omitempty"`
	MIME      string   `json:"mime,omitempty"`
	CreatedAt int64    `json:"createdAt"` // UnixNano
	LastOpen  int64    `json:"lastOpen"`  // UnixNano
	Keys      []string `json:"keys,omitempty"`
}

type indexDoc struct {
	Version int                   `json:"version"`
	Blobs   map[string]*blobEntry `json:"blobs"`
	Keys    map[string]string     `json:"keys,omitempty"`
}

type putFlight struct {
	done      chan struct{}
	ref       Ref
	err       error
	abandoned bool
}

// Namespace is one library's isolated blob cache directory and index.
type Namespace struct {
	cache      *Cache
	key        string
	libraryKey string
	dir        string

	mu                  sync.Mutex
	declared            Quota
	owner               *Quota
	indexInfo           os.FileInfo
	quota               Quota
	used                int64
	blobs               map[string]*blobEntry    // sha256 hex -> entry
	openCounts          map[string]int           // sha256 hex -> active Open handles
	keys                map[string]string        // caller key -> sha256 hex
	flights             map[string]*putFlight    // single-flight per caller key
	partials            map[string]*partialEntry // partial id -> tracked partial
	partialUsed         int64                    // total bytes staged in partials
	partialReservations map[string]int64         // active partial id -> remaining reserved bytes
	partialReserved     int64                    // sum of partialReservations
	partialLocks        map[string]chan struct{} // active partial writers by id
	dirty               bool
	lastSaved           time.Time
	tempSeq             atomic.Uint64

	saveMu sync.Mutex
}

func newEmptyNamespace(c *Cache, key string, quota Quota) *Namespace {
	if quota.MaxBytes <= 0 && !c.options.Unified {
		quota.MaxBytes = DefaultMaxBytes
	}
	return &Namespace{
		cache:      c,
		key:        key,
		libraryKey: key,
		dir:        filepath.Join(c.dir, key),
		declared:   quota, quota: quota,
		blobs:               make(map[string]*blobEntry),
		openCounts:          make(map[string]int),
		keys:                make(map[string]string),
		flights:             make(map[string]*putFlight),
		partials:            make(map[string]*partialEntry),
		partialReservations: make(map[string]int64),
		partialLocks:        make(map[string]chan struct{}),
	}
}

func (c *Cache) openNamespaceLocked(key string, quota Quota) (*Namespace, error) {
	if c.options.CheckVolume != nil {
		if err := c.options.CheckVolume(); err != nil {
			return nil, err
		}
	}
	ns := newEmptyNamespace(c, key, quota)
	if err := os.MkdirAll(ns.dir, 0o755); err != nil {
		return nil, err
	}
	if !c.options.Shared {
		ns.cleanStaleTempFiles()
		ns.loadIndex()
	} else if err := ns.refreshIndex(); err != nil {
		return nil, err
	}
	if !c.options.Shared {
		ns.loadPartials()
	}
	if err := ns.readOwnerQuota(); err != nil {
		return nil, err
	}
	return ns, nil
}

// cleanStaleTempFiles removes any leftover .part / .tmp files from previous
// runs (spec §4.3: a .part file older than the run is discarded).
func (n *Namespace) cleanStaleTempFiles() {
	_ = filepath.WalkDir(n.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(path)
		}
		return nil
	})
}

func (n *Namespace) indexPath() string {
	return filepath.Join(n.dir, "index.json")
}

func (n *Namespace) blobPath(hash string) string {
	if len(hash) < 2 {
		return filepath.Join(n.dir, hash)
	}
	return filepath.Join(n.dir, hash[:2], hash)
}

func (n *Namespace) loadIndex() {
	raw, err := os.ReadFile(n.indexPath())
	if err != nil {
		return
	}
	var doc indexDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var used int64
	for h, e := range doc.Blobs {
		if _, err := ParseRef("blob:" + h); e == nil || err != nil {
			continue
		}
		st, err := os.Stat(n.blobPath(h))
		if err != nil || st.IsDir() || st.Size() != e.Size {
			continue
		}
		e.Hash = h
		n.blobs[h] = e
		used += e.Size
		for _, k := range e.Keys {
			if k != "" {
				n.keys[k] = h
			}
		}
	}
	for k, h := range doc.Keys {
		if _, ok := n.blobs[h]; ok && k != "" {
			n.keys[k] = h
		}
	}
	n.used = used
}

func (n *Namespace) flushIndex() {
	if err := n.saveIndex(); err != nil {
		n.cache.logger.Error("save blob index", "err", err)
	}
}
func (n *Namespace) saveIndex() (err error) {
	defer func() {
		if err != nil {
			n.mu.Lock()
			n.dirty = true
			n.mu.Unlock()
		}
	}()
	n.saveMu.Lock()
	defer n.saveMu.Unlock()

	n.mu.Lock()
	if !n.dirty {
		n.mu.Unlock()
		return nil
	}
	doc := indexDoc{
		Version: 1,
		Blobs:   make(map[string]*blobEntry, len(n.blobs)),
		Keys:    make(map[string]string, len(n.keys)),
	}
	for h, e := range n.blobs {
		cp := *e
		cp.Keys = append([]string(nil), e.Keys...)
		doc.Blobs[h] = &cp
	}
	for k, h := range n.keys {
		if _, ok := doc.Blobs[h]; ok {
			doc.Keys[k] = h
		}
	}
	n.dirty = false
	n.lastSaved = n.cache.now()
	n.mu.Unlock()

	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(n.dir, 0o755); err != nil {
		return err
	}
	return atomicfile.Write(n.indexPath(), raw, 0644)
}

// SetQuota updates the manifest declaration, preserving the owner's override,
// and immediately sweeps if over budget.
func (n *Namespace) SetQuota(q Quota) {
	n.mu.Lock()
	n.declared = q
	n.applyQuotaLocked()
	n.mu.Unlock()
	n.Sweep(0)
}

// Quota returns the namespace's current quota.
func (n *Namespace) Quota() Quota {
	if n.cache.options.Shared {
		if unlock, err := n.cache.lockPool(context.Background()); err == nil {
			_ = n.readOwnerQuota()
			unlock()
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.quota
}

// Stats returns the current bytes (including staged resumable partials) and
// completed readable blob count in this namespace.
func (n *Namespace) Stats() (bytes int64, blobs int) {
	if n.cache.options.Shared {
		unlock, err := n.cache.readGuard()
		if err != nil {
			return 0, 0
		}
		defer unlock()
	}
	return n.storedStats()
}
func (n *Namespace) storedStats() (bytes int64, blobs int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	bytes = n.used + n.partialUsed
	if n.cache.options.Unified {
		for _, pe := range n.partials {
			bytes -= pe.Holes
		}
	}
	return bytes, len(n.blobs)
}

// LookupKey checks if a blob was stored under key, is not expired, and still
// exists on disk. If found, it returns its Ref ("blob:<sha256>").
func (n *Namespace) LookupKey(key string) (Ref, bool) {
	unlock, err := n.cache.guard(context.Background())
	if err != nil {
		return "", false
	}
	defer unlock()
	if key == "" {
		return "", false
	}
	now := n.cache.now()
	n.mu.Lock()
	h, ok := n.keys[key]
	if !ok {
		n.mu.Unlock()
		return "", false
	}
	e := n.blobs[h]
	if e == nil {
		delete(n.keys, key)
		n.mu.Unlock()
		return "", false
	}
	if n.isExpiredLocked(e, now) {
		n.mu.Unlock()
		n.sweep(0, "")
		return "", false
	}
	n.mu.Unlock()
	if _, err := os.Stat(n.blobPath(h)); err != nil {
		return "", false
	}
	return Ref("blob:" + h), true
}

// OpenByKey opens a blob previously stored under key, touching its LRU atime.
func (n *Namespace) OpenByKey(key string) (ByteSource, error) {
	ref, ok := n.LookupKey(key)
	if !ok {
		return nil, ErrNotFound
	}
	return n.Open(ref)
}

// Put streams r to a temporary file while computing its SHA-256, atomically
// renames it into `<libraryKey>/<sha256[:2]>/<sha256>`, records key -> ref, and
// sweeps the namespace and global cache if over quota.
//
// When size >= 0, Put verifies that the number of bytes read equals size.
// Concurrent Puts with the same non-empty key single-flight when the key is
// already cached or in flight.
func (n *Namespace) Put(ctx context.Context, key string, r io.Reader, size int64) (Ref, error) {
	return n.PutWithExpectedSHA256(ctx, key, r, size, "", "")
}

// PutWithExpectedSHA256 is Put with optional expected SHA-256 verification and
// MIME metadata. If expectedSHA256 is non-empty and does not match the stream's
// digest, the temp file is discarded and an error is returned.
func (n *Namespace) PutWithExpectedSHA256(ctx context.Context, key string, r io.Reader, size int64, expectedSHA256, mimeType string) (Ref, error) {
	if n.cache.options.Unified {
		return n.putUnified(ctx, key, r, size, expectedSHA256, mimeType)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	n.cache.mu.Lock()
	closed := n.cache.closed
	n.cache.mu.Unlock()
	if closed {
		return "", ErrClosed
	}

	n.mu.Lock()
	maxBytes := n.quota.EffectiveMaxBytes()
	n.mu.Unlock()
	sharedCap := n.cache.EffectiveSharedCap()
	limit := min(maxBytes, sharedCap)
	if limit <= 0 {
		return "", ErrTooLarge
	}
	if size > maxBytes || size > sharedCap {
		return "", fmt.Errorf("%w: size %d exceeds limit %d", ErrTooLarge, size, limit)
	}

	if err := os.MkdirAll(n.dir, 0o755); err != nil {
		return "", fmt.Errorf("blobcache: mkdir %s: %w", n.dir, err)
	}

	tmpPath := filepath.Join(n.dir, fmt.Sprintf(".put-%d.part", n.tempSeq.Add(1)))
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("blobcache: create temp: %w", err)
	}
	removeTmp := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	h := sha256.New()
	var written int64
	var head [512]byte
	headLen := 0
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			removeTmp()
			return "", err
		}
		nr, rerr := r.Read(buf)
		if nr > 0 {
			written += int64(nr)
			if written > maxBytes || written > sharedCap {
				removeTmp()
				return "", fmt.Errorf("%w: stream exceeded %d bytes", ErrTooLarge, limit)
			}
			if headLen < len(head) {
				copied := copy(head[headLen:], buf[:nr])
				headLen += copied
			}
			if _, werr := f.Write(buf[:nr]); werr != nil {
				removeTmp()
				return "", fmt.Errorf("blobcache: write temp: %w", werr)
			}
			_, _ = h.Write(buf[:nr])
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			removeTmp()
			return "", rerr
		}
	}
	if err := f.Close(); err != nil {
		removeTmp()
		return "", fmt.Errorf("blobcache: close temp: %w", err)
	}
	if size >= 0 && written != size {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("blobcache: size mismatch: got %d bytes, want %d", written, size)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	if expectedSHA256 != "" && !strings.EqualFold(strings.TrimSpace(expectedSHA256), digest) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("blobcache: sha256 mismatch: got %s, want %s", digest, expectedSHA256)
	}

	finalPath := n.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("blobcache: mkdir shard: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("blobcache: rename blob: %w", err)
	}

	nowNs := n.cache.now().UnixNano()
	n.mu.Lock()
	e, exists := n.blobs[digest]
	if !exists {
		e = &blobEntry{
			Hash:      digest,
			Size:      written,
			Name:      filepath.Base(key),
			MIME:      mimeType,
			CreatedAt: nowNs,
			LastOpen:  nowNs,
		}
		n.blobs[digest] = e
		n.used += written
	} else {
		if e.Size != written {
			n.used += written - e.Size
			e.Size = written
		}
		e.CreatedAt = nowNs
		e.LastOpen = nowNs
		if e.Name == "" && key != "" {
			e.Name = filepath.Base(key)
		}
		if mimeType != "" {
			e.MIME = mimeType
		}
	}
	if key != "" {
		n.keys[key] = digest
		if !containsString(e.Keys, key) {
			e.Keys = append(e.Keys, key)
		}
	}
	n.dirty = true
	overNamespace := n.used+n.partialUsed+n.partialReserved > n.quota.EffectiveMaxBytes()
	n.mu.Unlock()

	if overNamespace {
		n.sweep(0, digest)
	}
	n.cache.enforceGlobalLimitProtected(0, n, digest)
	n.flushIndex()
	return Ref("blob:" + digest), nil
}

// SingleFlightPut returns an existing cached blob for key if present, or runs
// fn once across concurrent callers for the same key. If the leader cancels or
// panics, waiting callers with live contexts retry cleanly instead of hanging
// or inheriting a dead flight.
func (n *Namespace) SingleFlightPut(ctx context.Context, key string, fn func(ctx context.Context) (Ref, error)) (Ref, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if ref, ok := n.LookupKey(key); ok {
			return ref, nil
		}
		n.mu.Lock()
		if fl, ok := n.flights[key]; ok {
			n.mu.Unlock()
			select {
			case <-fl.done:
				if fl.abandoned {
					continue
				}
				return fl.ref, fl.err
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		fl := &putFlight{done: make(chan struct{})}
		n.flights[key] = fl
		n.mu.Unlock()

		ref, err := func() (ref Ref, err error) {
			defer func() {
				if r := recover(); r != nil {
					ref = ""
					err = fmt.Errorf("blobcache: singleflight panic: %v", r)
				}
				fl.ref = ref
				fl.err = err
				fl.abandoned = err != nil && ctx.Err() != nil
				n.mu.Lock()
				if n.flights[key] == fl {
					delete(n.flights, key)
				}
				n.mu.Unlock()
				close(fl.done)
			}()
			return fn(ctx)
		}()
		return ref, err
	}
}

// Open opens a cached blob by its reference ("blob:<sha256>"), updating its
// last-open timestamp in the index.
func (n *Namespace) Open(ref Ref) (ByteSource, error) {
	return n.open(ref, true)
}

func (n *Namespace) open(ref Ref, recordAccess bool) (ByteSource, error) {
	unlock, err := n.cache.guard(context.Background())
	if err != nil {
		return nil, err
	}
	defer unlock()
	hash, err := ref.Hash()
	if err != nil {
		return nil, err
	}
	now := n.cache.now()
	nowNs := now.UnixNano()

	n.mu.Lock()
	e := n.blobs[hash]
	if e == nil {
		n.mu.Unlock()
		return nil, ErrNotFound
	}
	if n.isExpiredLocked(e, now) {
		isOpen := n.isOpenLocked(hash)
		n.mu.Unlock()
		if recordAccess && !isOpen {
			n.evictHash(hash)
			n.flushIndex()
		}
		return nil, ErrNotFound
	}
	path := n.blobPath(hash)
	f, err := os.Open(path)
	if err == nil {
		info, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			n.mu.Unlock()
			return nil, statErr
		}
		if !info.Mode().IsRegular() || info.Size() != e.Size {
			f.Close()
			if recordAccess && !n.isOpenLocked(hash) {
				_, _ = n.removeFileLocked(hash)
			}
			n.mu.Unlock()
			if recordAccess {
				n.flushIndex()
			}
			return nil, ErrNotFound
		}
	}
	if err != nil {
		if recordAccess && errors.Is(err, fs.ErrNotExist) && !n.isOpenLocked(hash) {
			n.dropEntryLocked(hash)
			n.dirty = true
		}
		n.mu.Unlock()
		if errors.Is(err, fs.ErrNotExist) {
			if recordAccess {
				n.flushIndex()
			}
			return nil, ErrNotFound
		}
		return nil, err
	}
	var lease *os.File
	if n.cache.options.Shared {
		lease, err = n.lease("read", hash, false)
		if err != nil {
			f.Close()
			n.mu.Unlock()
			return nil, err
		}
	}
	n.openCounts[hash]++
	if recordAccess {
		e.LastOpen = nowNs
		n.dirty = true
	}
	shouldFlush := recordAccess && now.Sub(n.lastSaved) >= atimeSaveDebounce
	name := e.Name
	if name == "" {
		name = hash
	}
	mimeType := e.MIME
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	size := e.Size
	libKey := n.libraryKey
	n.mu.Unlock()

	if shouldFlush || (recordAccess && n.cache.options.Shared) {
		n.flushIndex()
	}
	return &fileSource{
		lease: lease, f: f,
		size:       size,
		name:       name,
		mime:       mimeType,
		libraryKey: libKey,
		ns:         n,
		hash:       hash,
	}, nil
}

func (n *Namespace) isOpenLocked(hash string) bool {
	return n.openCounts[hash] > 0 || n.remoteBusy("read", hash)
}

func (n *Namespace) releaseOpen(hash string) {
	n.mu.Lock()
	if c := n.openCounts[hash]; c <= 1 {
		delete(n.openCounts, hash)
	} else {
		n.openCounts[hash] = c - 1
	}
	n.mu.Unlock()
	if n.cache.options.Shared {
		unlock, err := n.cache.lockPool(context.Background())
		if err != nil {
			return
		}
		defer unlock()
		if !n.remoteBusy("read", hash) {
			_ = os.Remove(n.leasePath("read", hash))
		}
	}
}

func (n *Namespace) isExpiredLocked(e *blobEntry, now time.Time) bool {
	if n.quota.TTL <= 0 || e.CreatedAt <= 0 {
		return false
	}
	created := time.Unix(0, e.CreatedAt)
	return !now.Before(created.Add(n.quota.TTL))
}

func (n *Namespace) dropEntryLocked(hash string) int64 {
	e, ok := n.blobs[hash]
	if !ok {
		return 0
	}
	delete(n.blobs, hash)
	n.used = max(n.used-e.Size, 0)
	for _, k := range e.Keys {
		if n.keys[k] == hash {
			delete(n.keys, k)
		}
	}
	return e.Size
}

func (n *Namespace) evictHash(hash string) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	size, err := n.removeFileLocked(hash)
	if err != nil && !errors.Is(err, ErrFileInUse) && !errors.Is(err, ErrNotFound) {
		n.cache.logger.Warn("remove cache file", "err", err)
	}
	return size
}

// removeFileLocked unlinks before releasing capacity. The volume lock keeps
// peer opens out until both the file and its index entry have been removed.
func (n *Namespace) removeFileLocked(hash string) (int64, error) {
	if n.blobs[hash] == nil {
		return 0, ErrNotFound
	}
	if n.isOpenLocked(hash) {
		return 0, ErrFileInUse
	}
	if err := os.Remove(n.blobPath(hash)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if n.cache.options.Shared {
		_ = os.Remove(n.leasePath("read", hash))
	}
	n.dirty = true
	return n.dropEntryLocked(hash), nil
}

// Delete removes one cached blob by its reference ("blob:<sha256>") from this
// namespace, freeing its disk file and quota accounting immediately. Deleting
// an absent or already-evicted blob returns (false, nil).
func (n *Namespace) Delete(ref Ref) (bool, error) {
	unlock, err := n.cache.guard(context.Background())
	if err != nil {
		return false, err
	}
	defer unlock()
	n.cache.mu.Lock()
	closed := n.cache.closed
	n.cache.mu.Unlock()
	if closed {
		return false, ErrClosed
	}
	hash, err := ref.Hash()
	if err != nil {
		return false, err
	}
	n.mu.Lock()
	_, err = n.removeFileLocked(hash)
	n.mu.Unlock()
	if errors.Is(err, ErrFileInUse) || errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, n.saveIndex()
}

// Sweep evicts TTL-expired blobs and idle partials, then LRU-evicts idle
// partials and least-recently-opened non-open blobs until
// used + partialUsed + partialReserved + need <= maxBytes.
// Currently open blobs and active partials are never evicted.
func (n *Namespace) Sweep(need int64) {
	unlock, err := n.cache.guard(context.Background())
	if err != nil {
		return
	}
	defer unlock()
	n.sweep(need, "")
}

func (n *Namespace) sweep(need int64, protectHash string) {
	if need < 0 {
		need = 0
	}
	now := n.cache.now()
	n.mu.Lock()
	maxBytes := n.quota.EffectiveMaxBytes()
	var expired []string
	var active []*blobEntry
	for h, e := range n.blobs {
		if n.isOpenLocked(h) || h == protectHash {
			continue
		}
		if n.isExpiredLocked(e, now) {
			expired = append(expired, h)
		} else {
			cp := *e
			active = append(active, &cp)
		}
	}
	for _, h := range expired {
		_, _ = n.removeFileLocked(h)
	}
	target := maxBytes
	if need <= maxBytes {
		target = maxBytes - need
	}
	n.sweepPartialsLocked(now, target)
	if n.used+n.partialUsed+n.partialReserved > target {
		sort.Slice(active, func(i, j int) bool {
			if active[i].LastOpen != active[j].LastOpen {
				return active[i].LastOpen < active[j].LastOpen
			}
			return active[i].Hash < active[j].Hash
		})
		for _, e := range active {
			if n.used+n.partialUsed+n.partialReserved <= target {
				break
			}
			_, _ = n.removeFileLocked(e.Hash)
		}
	}
	dirty := n.dirty
	n.mu.Unlock()

	if dirty {
		n.flushIndex()
	}
}

// Purge removes all cached blobs and index state for this library namespace
// (used on plugin removal, spec §2.5, §5.4).
func (n *Namespace) Purge() {
	if n.cache.options.Unified {
		unlock, err := n.cache.guard(context.Background())
		if err != nil {
			return
		}
		defer unlock()
		n.clearEligible()
		return
	}
	n.saveMu.Lock()
	defer n.saveMu.Unlock()

	n.mu.Lock()
	n.blobs = make(map[string]*blobEntry)
	n.openCounts = make(map[string]int)
	n.keys = make(map[string]string)
	n.partials = make(map[string]*partialEntry)
	n.partialReservations = make(map[string]int64)
	n.used = 0
	n.partialUsed = 0
	n.partialReserved = 0
	n.dirty = false
	dir := n.dir
	n.mu.Unlock()

	_ = os.RemoveAll(dir)
}

func containsString(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}

// fileSource implements ByteSource over an *os.File.
type fileSource struct {
	lease      *os.File
	f          *os.File
	size       int64
	name       string
	mime       string
	libraryKey string
	ns         *Namespace
	hash       string
	closeOnce  sync.Once
	closeErr   error
}

func (s *fileSource) ReadAt(p []byte, off int64) (int, error) {
	return s.f.ReadAt(p, off)
}

func (s *fileSource) Read(p []byte) (int, error) {
	return s.f.Read(p)
}

func (s *fileSource) Seek(offset int64, whence int) (int64, error) {
	return s.f.Seek(offset, whence)
}

func (s *fileSource) Size() int64        { return s.size }
func (s *fileSource) Name() string       { return s.name }
func (s *fileSource) MIME() string       { return s.mime }
func (s *fileSource) LibraryKey() string { return s.libraryKey }
func (s *fileSource) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.f.Close()
		if s.lease != nil {
			s.lease.Close()
		}
		if s.ns != nil && s.hash != "" {
			s.ns.releaseOpen(s.hash)
		}
	})
	return s.closeErr
}

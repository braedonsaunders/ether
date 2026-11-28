// Whole-file entries in the media pool. Only admission and index updates take
// the pool lock. File bodies stream independently; open readers and writers
// hold file leases released by the kernel if the serving process exits.
package blobcache

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/atomicfile"
	"github.com/braedonsaunders/ether/internal/segcache"
)

// FileInfo describes one readable, content-addressed entry in a library.
type FileInfo struct {
	LibraryKey string
	Name       string
	Ref        Ref
	Size       int64
	LastOpen   time.Time
	InUse      bool
}

func (c *Cache) lockPool(ctx context.Context) (func(), error) {
	if !c.options.Unified && !c.options.Shared {
		return func() {}, nil
	}
	if c.options.CheckVolume != nil {
		if e := c.options.CheckVolume(); e != nil {
			return nil, e
		}
	}
	c.poolMu.Lock()
	if !c.options.Shared {
		return c.poolMu.Unlock, nil
	}
	f, err := os.OpenFile(filepath.Join(c.dir, ".allocation.lock"), os.O_RDWR, 0)
	if err != nil {
		c.poolMu.Unlock()
		return nil, err
	}
	for {
		ok, e := tryFileLock(f, true)
		if ok {
			break
		}
		if e != nil {
			err = e
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if err != nil {
			break
		}
	}

	if err == nil && c.options.CheckVolume != nil {
		err = c.options.CheckVolume()
	}
	unlock := func() { f.Close(); c.poolMu.Unlock() }
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// An admission holds the volume lock across both the segment and file decisions.
// Any file mutation invalidates the idle usage snapshot before changing disk.
func (c *Cache) guard(ctx context.Context) (func(), error) {
	unlock, err := c.lockPool(ctx)
	if err != nil {
		return nil, err
	}
	if c.options.Shared {
		if err = c.refreshForMutation(); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

func (c *Cache) readGuard() (func(), error) {
	unlock, err := c.lockPool(context.Background())
	if err != nil {
		return nil, err
	}
	if c.options.Shared {
		if err = c.refreshShared(); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

func (c *Cache) refreshForMutation() error {
	err := os.Remove(filepath.Join(c.dir, ".idle-usage"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return c.refreshShared()
}

func (c *Cache) listNamespaces() []*Namespace {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := make([]*Namespace, 0, len(c.namespaces))
	for _, ns := range c.namespaces {
		list = append(list, ns)
	}
	return list
}

func (c *Cache) refreshShared() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		n := c.Namespace(entry.Name(), Quota{})
		if err := n.refreshIndex(); err != nil {
			return err
		}
		if err := n.recoverPublication(); err != nil {
			return err
		}
		if err := n.refreshPartials(); err != nil {
			return err
		}
		if err := n.readOwnerQuota(); err != nil {
			return err
		}
	}
	return nil
}

func (n *Namespace) refreshIndex() error {
	info, err := os.Stat(n.indexPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if info != nil && n.indexInfo != nil && os.SameFile(info, n.indexInfo) && info.ModTime().Equal(n.indexInfo.ModTime()) {
		return nil
	}
	var d indexDoc
	if info != nil {
		b, err := os.ReadFile(n.indexPath())
		if err != nil {
			return err
		}
		if err = json.Unmarshal(b, &d); err != nil {
			return err
		}
		if d.Version != 1 {
			return fmt.Errorf("unrecognized media cache index version %d", d.Version)
		}
	}
	if d.Blobs == nil {
		d.Blobs = map[string]*blobEntry{}
	}
	if d.Keys == nil {
		d.Keys = map[string]string{}
	}
	n.used = 0
	for h, e := range d.Blobs {
		if _, err := ParseRef("blob:" + h); err != nil || e == nil || e.Size < 0 {
			return errors.New("invalid media cache index")
		}
		e.Hash = h
		n.used += e.Size
	}
	n.blobs = d.Blobs
	n.keys = d.Keys
	n.indexInfo = info
	n.dirty = false
	return nil
}

// A process may exit between publishing a verified body and replacing its
// index. Recover that single pending publication under the volume lock.
func (n *Namespace) recoverPublication() error {
	marker := filepath.Join(n.dir, ".publishing")
	data, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	h, err := ParseRef(string(data))
	if err != nil {
		return err
	}
	// The failed writer may still have an uncommitted in-memory entry.
	n.mu.Lock()
	n.indexInfo = nil
	n.mu.Unlock()
	if err := n.refreshIndex(); err != nil {
		return err
	}
	n.mu.Lock()
	known := n.blobs[h] != nil
	n.mu.Unlock()
	if !known {
		if err = os.Remove(n.blobPath(h)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Remove(marker)
}

func (n *Namespace) leasePath(kind, id string) string {
	return filepath.Join(n.dir, ".leases", kind+"-"+id)
}

func (n *Namespace) lease(kind, id string, exclusive bool) (*os.File, error) {
	path := n.leasePath(kind, id)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	ok, err := tryFileLock(f, exclusive)
	if ok {
		return f, nil
	}
	f.Close()
	if err == nil {
		err = ErrFileInUse
	}
	return nil, err
}

// ErrFileInUse indicates an active reader or writer protects the requested entry.
var ErrFileInUse = errors.New("cache file in use")

func (n *Namespace) remoteBusy(kind, id string) bool {
	if !n.cache.options.Shared {
		return false
	}
	f, err := os.OpenFile(n.leasePath(kind, id), os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close()
	idle, _ := tryFileLock(f, true)
	return !idle
}

func (n *Namespace) refreshPartials() error {
	dir, err := os.OpenRoot(n.partialsDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for id := range n.partials {
		if n.partialLocks[id] == nil {
			n.updatePartialLocked(id, 0, 0)
		}
	}
	for id := range n.partialReservations {
		if n.partialLocks[id] == nil {
			n.setPartialReservationLocked(id, 0)
		}
	}
	var claims []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".claim"); ok && len(id) == 64 && n.partialLocks[id] == nil {
			claims = append(claims, id)
		}
		id, ok := strings.CutSuffix(e.Name(), ".partial")
		if !ok || len(id) != 64 || n.partialLocks[id] != nil {
			continue
		}
		// On Windows, directory entries can report the pre-write size of
		// open files. Lstat on the root queries the file by handle and
		// shares delete access with the writer.
		st, err := dir.Lstat(e.Name())
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		n.updatePartialLocked(id, st.Size(), st.ModTime().UnixNano())
	}
	for _, id := range claims {
		path := filepath.Join(n.partialsDir(), id+".claim")
		if !n.remoteBusy("write", id) {
			_ = os.Remove(path)
			continue
		}
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		size, ranged := strings.CutSuffix(string(raw), " range")
		total, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return err
		}
		if e := n.partials[id]; e != nil {
			total -= e.Size
			if ranged {
				// Checkpoints may trail in-flight writes, conservatively keeping
				// the not-yet-checkpointed bytes reserved against disk headroom.
				data, err := os.ReadFile(n.partialMetaPath(id))
				if err != nil && !os.IsNotExist(err) {
					return err
				}
				var meta PartialMeta
				if err == nil {
					if err = json.Unmarshal(data, &meta); err != nil {
						return err
					}
				}
				e.Holes = max(e.Size-meta.Size, 0)
			}
		}
		n.setPartialReservationLocked(id, max(total, 0))
	}
	return nil
}

// busyReadersLocked only probes known reader lease files. Unopened entries
// never create or acquire lease files during owner statistics polling.
func (n *Namespace) busyReadersLocked() map[string]bool {
	busy := make(map[string]bool, len(n.openCounts))
	for h := range n.openCounts {
		busy[h] = true
	}
	if n.cache.options.Shared {
		entries, err := os.ReadDir(filepath.Join(n.dir, ".leases"))
		if err != nil && !os.IsNotExist(err) {
			for h := range n.blobs {
				busy[h] = true
			}
			return busy
		}
		for _, e := range entries {
			h, ok := strings.CutPrefix(e.Name(), "read-")
			if ok && n.blobs[h] != nil && n.remoteBusy("read", h) {
				busy[h] = true
			}
		}
	}
	return busy
}

// ProtectedBytes reports resident bytes protected by active readers or writers.
func (c *Cache) ProtectedBytes() int64 {
	unlock, err := c.readGuard()
	if err != nil {
		return 0
	}
	defer unlock()
	var bytes int64
	for _, n := range c.listNamespaces() {
		n.mu.Lock()
		for h := range n.busyReadersLocked() {
			if e := n.blobs[h]; e != nil {
				bytes += e.Size
			}
		}
		for id, pe := range n.partials {
			if n.partialLocks[id] != nil || n.remoteBusy("write", id) {
				bytes += pe.Size - pe.Holes
			}
		}
		n.mu.Unlock()
	}
	return bytes
}

func (c *Cache) filesLocked() []FileInfo {
	var out []FileInfo
	for _, n := range c.listNamespaces() {
		n.mu.Lock()
		busy := n.busyReadersLocked()
		for h, e := range n.blobs {
			out = append(out, FileInfo{n.libraryKey, e.Name, Ref("blob:" + h), e.Size, time.Unix(0, e.LastOpen), busy[h]})
		}
		n.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastOpen.Before(out[j].LastOpen) })
	return out
}

func (c *Cache) Files() []FileInfo {
	u, e := c.readGuard()
	if e != nil {
		return nil
	}
	defer u()
	return c.filesLocked()
}

func (c *Cache) EvictFile(key string, ref Ref) (int64, error) {
	u, e := c.guard(context.Background())
	if e != nil {
		return 0, e
	}
	defer u()
	h, e := ref.Hash()
	if e != nil {
		return 0, e
	}
	n := c.Namespace(key, Quota{})
	n.mu.Lock()
	v, err := n.removeFileLocked(h)
	n.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return v, n.saveIndex()
}

// ClearFiles removes complete and interrupted transfers that are idle. Active
// readers and reservations remain visible and keep their capacity charge.
func (c *Cache) ClearFiles() (int64, error) {
	unlock, err := c.guard(context.Background())
	if err != nil {
		return 0, err
	}
	defer unlock()
	var freed int64
	var failures []error
	for _, n := range c.listNamespaces() {
		bytes, err := n.clearEligible()
		freed += bytes
		if err != nil {
			failures = append(failures, err)
		}
	}
	return freed, errors.Join(failures...)
}

func (n *Namespace) clearEligible() (int64, error) {
	var freed int64
	var failures []error
	n.mu.Lock()
	for h := range n.blobs {
		size, err := n.removeFileLocked(h)
		freed += size
		if err != nil {
			failures = append(failures, err)
		}
	}
	for id, pe := range n.partials {
		if n.partialLocks[id] != nil || n.remoteBusy("write", id) {
			failures = append(failures, ErrFileInUse)
			continue
		}
		if size := n.removePartialLocked(pe); size > 0 {
			freed += size
		} else {
			failures = append(failures, errors.New("could not remove staged file"))
		}
	}
	if n.partialReserved > 0 {
		failures = append(failures, ErrFileInUse)
	}
	n.mu.Unlock()
	failures = append(failures, n.saveIndex())
	return freed, errors.Join(failures...)
}

func (c *Cache) AccountedBytes() int64 {
	a, b, e := c.FileUsage()
	if e != nil {
		return 0
	}
	return a + b
}

func (c *Cache) FileUsage() (int64, int64, error) {
	var retained, reserved int64
	err := c.WithAllocation(func(accounted, unwritten int64, _ segcache.FileCandidates) error {
		retained, reserved = accounted-unwritten, unwritten
		return nil
	})
	return retained, reserved, err
}

func (c *Cache) usage() (accounted, reserved int64) {
	for _, n := range c.listNamespaces() {
		n.mu.Lock()
		accounted += n.used + n.partialUsed + n.partialReserved
		reserved += n.partialReserved
		for _, pe := range n.partials {
			reserved += pe.Holes
		}
		n.mu.Unlock()
	}
	return
}

func (c *Cache) WithAllocation(fn func(int64, int64, segcache.FileCandidates) error) error {
	unlock, err := c.lockPool(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	if !c.options.Shared {
		a, r := c.usage()
		return fn(a, r, c.fileCandidates)
	}
	// In steady-state, segment fills require one small counter read regardless
	// of the number of indexed games or libraries on the volume.
	path := filepath.Join(c.dir, ".idle-usage")
	if data, err := os.ReadFile(path); err == nil {
		if accounted, err := strconv.ParseInt(string(data), 10, 64); err == nil && accounted >= 0 {
			return fn(accounted, 0, func() ([]segcache.FileCandidate, error) {
				if err := c.refreshForMutation(); err != nil {
					return nil, err
				}
				return c.fileCandidates()
			})
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := c.refreshShared(); err != nil {
		return err
	}
	a, r := c.usage()
	if r == 0 {
		gate, err := os.OpenFile(filepath.Join(c.dir, ".staging.lock"), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		idle, err := tryFileLock(gate, true)
		gate.Close()
		if err != nil {
			return err
		}
		if idle {
			if err := atomicfile.Write(path, []byte(strconv.FormatInt(a, 10)), 0600); err != nil {
				return err
			}
		}
	}
	return fn(a, r, func() ([]segcache.FileCandidate, error) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return c.fileCandidates()
	})
}

// fileCandidates snapshots only when an admission needs eviction. Callers
// retain the allocation lock while merging these entries with segment ages.
func (c *Cache) fileCandidates() ([]segcache.FileCandidate, error) {
	var candidates []segcache.FileCandidate
	for _, n := range c.listNamespaces() {
		n.mu.Lock()
		busy := n.busyReadersLocked()
		for h, e := range n.blobs {
			if busy[h] || e.Size == 0 {
				continue
			}
			candidates = append(candidates, segcache.FileCandidate{LastUsed: time.Unix(0, e.LastOpen), Evict: func() (int64, error) {
				n.mu.Lock()
				size, err := n.removeFileLocked(h)
				n.mu.Unlock()
				if errors.Is(err, ErrFileInUse) || errors.Is(err, ErrNotFound) {
					return 0, nil
				}
				if err != nil {
					return 0, err
				}
				return size, n.saveIndex()
			}})
		}
		for id, pe := range n.partials {
			if n.partialLocks[id] != nil || n.remoteBusy("write", id) {
				continue
			}
			candidates = append(candidates, segcache.FileCandidate{LastUsed: time.Unix(0, pe.UpdatedAt), Evict: func() (int64, error) {
				return n.evictPartial(id), nil
			}})
		}
		n.mu.Unlock()
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].LastUsed.Before(candidates[j].LastUsed) })
	return candidates, nil
}

func (n *Namespace) applyQuotaLocked() {
	n.quota = n.declared
	if n.owner != nil {
		if n.owner.MaxBytes > 0 {
			n.quota.MaxBytes = n.owner.MaxBytes
		}
		n.quota.TTL = n.owner.TTL
	}
	if !n.cache.options.Unified && n.quota.MaxBytes <= 0 {
		n.quota.MaxBytes = DefaultMaxBytes
	}
}

func (n *Namespace) readOwnerQuota() error {
	raw, err := os.ReadFile(filepath.Join(n.dir, "quota.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var q Quota
	if err = json.Unmarshal(raw, &q); err != nil {
		return err
	}
	if q.MaxBytes < 0 || q.TTL < 0 {
		return errors.New("invalid stored library quota")
	}
	n.mu.Lock()
	n.owner = &q
	n.applyQuotaLocked()
	n.mu.Unlock()
	return nil
}

// SetOwnerQuota persists an owner override independently of the manifest.
func (n *Namespace) SetOwnerQuota(q Quota) error {
	if q.MaxBytes < 0 || q.TTL < 0 {
		return errors.New("invalid library quota")
	}
	u, e := n.cache.guard(context.Background())
	if e != nil {
		return e
	}
	defer u()
	data, e := json.Marshal(q)
	if e != nil {
		return e
	}
	p := filepath.Join(n.dir, "quota.json")
	if e = atomicfile.Write(p, data, 0600); e != nil {
		return e
	}
	n.mu.Lock()
	n.owner = &q
	n.applyQuotaLocked()
	n.mu.Unlock()
	n.sweep(0, "")
	return nil
}

// AddAliases attaches unclaimed legacy keys to a verified entry during migration.
func (n *Namespace) AddAliases(ref Ref, keys []string, displayName ...string) error {
	h, e := ref.Hash()
	if e != nil {
		return e
	}
	u, e := n.cache.guard(context.Background())
	if e != nil {
		return e
	}
	defer u()
	n.mu.Lock()
	entry := n.blobs[h]
	if entry == nil {
		n.mu.Unlock()
		return ErrNotFound
	}
	if len(displayName) > 0 && displayName[0] != "" && (entry.Name == "" || entry.Name == ".") {
		entry.Name = displayName[0]
		n.dirty = true
	}
	for _, k := range keys {
		if k != "" && n.keys[k] == "" {
			n.keys[k] = h
			entry.Keys = append(entry.Keys, k)
			n.dirty = true
		}
	}
	n.mu.Unlock()
	return n.saveIndex()
}

func (n *Namespace) putUnified(ctx context.Context, key string, r io.Reader, size int64, expected, mime string) (Ref, error) {
	var id [16]byte
	rand.Read(id[:])
	p, err := n.OpenPartial(ctx, fmt.Sprintf("put-%x", id))
	if err != nil {
		return "", err
	}
	defer p.Close()
	defer func() {
		if !p.committed {
			_ = p.Discard()
		}
	}()
	if err = p.Reserve(size); err != nil {
		return "", err
	}
	var src io.Reader = &ctxReader{ctx, r}
	if size >= 0 {
		// A dishonest length must not displace other files before the mismatch
		// is noticed. Reserve rejected impossible sizes before they reach here.
		src = &boundedUploadReader{ctx: ctx, r: r, left: size}
	}
	stat, err := copyWithDigests(p, src)
	if err != nil {
		return "", err
	}
	if size >= 0 && p.size != size {
		return "", fmt.Errorf("blobcache: size mismatch: got %d, expected %d", p.size, size)
	}
	if expected != "" && !strings.EqualFold(strings.TrimSpace(expected), stat.SHA256) {
		return "", errors.New("blobcache: sha256 mismatch")
	}
	result, err := p.commit(ctx, key, mime, &stat)
	return result.Ref, err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *ctxReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}

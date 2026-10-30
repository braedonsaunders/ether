package playback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

const payloadNamespace = "playback-source"
const payloadWitnessSlots = 128
const payloadRecoveryBatch = 40

// Unknown Close/commit responsibility must survive dropping the Manager itself.
// Only confirmed retirement releases these process roots; OS death releases locks.
var heldPayloadWitnesses sync.Map

var errPayloadCommitUnknown = errors.New("playback: payload ownership commit outcome unknown")

var errPayloadWitnessCapacity = errors.New("playback: all 128 payload ownership witness slots are held or unreconciled")
var errPayloadFilesystem = errors.New("playback: payload ownership requires a verified local filesystem; network or unknown filesystems cannot authorize reclamation")

type payloadAnchor struct {
	owner    payloadOwner
	refs     int
	retiring bool
}

type payloadStore struct {
	recoveryRunning          bool
	recoveryCancel           context.CancelFunc
	gate                     chan struct{}
	anchors                  map[string]*payloadAnchor
	retired                  map[string]payloadOwner
	releaseRunning           bool
	releaseStop              chan struct{}
	db                       *sql.DB
	dir, domain, incarnation string
	file                     *os.File
	mu                       sync.Mutex
	active                   int
	closing                  bool
	cursor                   int
	afterOwnerCommit         func() error
	afterRemove              func() error
}
type payloadOwnerState struct {
	id       string
	anchor   *payloadAnchor
	mu       sync.Mutex
	released bool
}

type payloadOwner struct {
	state                     *payloadOwnerState
	header                    capability.Header
	domain, incarnation, hash string
}

func validPayloadID(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
func verifyWitnessFile(f *os.File) error {
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	path, err := os.Lstat(f.Name())
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !path.Mode().IsRegular() || !os.SameFile(opened, path) {
		return core.ErrNotAvailable
	}
	return nil
}
func writeWitness(f *os.File, value string) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt([]byte(value), 0); err != nil {
		return err
	}
	return f.Sync()
}
func readWitness(f *os.File) (string, error) {
	b, err := io.ReadAll(io.NewSectionReader(f, 0, 256))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
func openPayloadStore(ctx context.Context, db *sql.DB, dir string) (*payloadStore, error) {
	reliable, err := payloadFilesystemReliable(dir)
	if err != nil {
		return nil, err
	}
	if !reliable {
		return nil, errPayloadFilesystem
	}
	locks := filepath.Join(dir, ".owners")
	if err := os.MkdirAll(locks, 0700); err != nil {
		return nil, err
	}
	domainFile, err := os.OpenFile(filepath.Join(dir, ".domain"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer domainFile.Close()
	if err := lockPayloadWitness(domainFile); err != nil {
		return nil, fmt.Errorf("playback: payload domain lock: %w", err)
	}
	if err := verifyWitnessFile(domainFile); err != nil {
		return nil, err
	}
	domain, err := readWitness(domainFile)
	if err != nil {
		return nil, err
	}
	if domain == "" {
		domain = randomID(32)
		if err := writeWitness(domainFile, domain); err != nil {
			return nil, err
		}
	}
	if !validPayloadID(domain) {
		return nil, errors.New("playback: invalid payload domain witness")
	}
	// The domain lock only serializes identity creation, not backend lifetimes.
	domainFile.Close()
	p := &payloadStore{gate: make(chan struct{}, 1), anchors: make(map[string]*payloadAnchor), releaseStop: make(chan struct{}), db: db, dir: dir, domain: domain, incarnation: randomID(32)}
	recovery := 0
	for slot := 0; slot < payloadWitnessSlots; slot++ {
		f, err := os.OpenFile(filepath.Join(locks, fmt.Sprintf("%03d.lock", slot)), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err := lockPayloadWitness(f); err != nil {
			f.Close()
			continue
		}
		if err := verifyWitnessFile(f); err != nil {
			f.Close()
			continue
		}
		value, err := readWitness(f)
		if err != nil {
			f.Close()
			continue
		}
		if value != "" {
			parts := strings.Split(value, "\n")
			if len(parts) != 2 || parts[0] != domain || !validPayloadID(parts[1]) {
				f.Close()
				continue
			}
			if recovery >= 4 {
				f.Close()
				continue
			}
			recovery++
			clear, err := p.recoverIncarnation(ctx, parts[1], f)
			if err != nil {
				f.Close()
				return nil, err
			}
			if !clear {
				f.Close()
				continue
			}
		}
		// Any registry whose witness is absent/mismatched remains protected.
		var occupied int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_payload_incarnations WHERE domain=? AND witness_slot=?`, domain, slot).Scan(&occupied); err != nil {
			f.Close()
			return nil, err
		}
		if occupied != 0 {
			f.Close()
			continue
		}
		if err := writeWitness(f, domain+"\n"+p.incarnation); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO playback_payload_incarnations(domain,incarnation,witness_slot) VALUES(?,?,?)`, domain, p.incarnation, slot); err != nil {
			f.Close()
			return nil, err
		}
		p.file = f
		heldPayloadWitnesses.Store(p.incarnation, p)
		return p, nil
	}
	return nil, errPayloadWitnessCapacity
}

func (p *payloadStore) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closing {
		if p.recoveryCancel != nil {
			p.recoveryCancel()
		}
		p.closing = true
		close(p.releaseStop)
	}
	if p.active == 0 && p.file != nil {
		p.file.Close()
		p.file = nil
		heldPayloadWitnesses.Delete(p.incarnation)
	}
}
func (p *payloadStore) begin() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing || p.file == nil || p.active >= 4096 {
		return core.ErrNotAvailable
	}
	p.active++
	return nil
}
func (p *payloadStore) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
	if p.closing && p.active == 0 && p.file != nil {
		p.file.Close()
		p.file = nil
		heldPayloadWitnesses.Delete(p.incarnation)
	}
}
func payloadLock(ctx context.Context, db *sql.DB, tx *sql.Tx, domain, hash string) error {
	return store.LockKey(ctx, store.DialectOf(db), tx, "playback:payload:"+domain+":"+hash)
}

// reserve commits protection before any file creation/backend Open. A failed
// commit is uncertain: its witness stays held until process death/reconciliation.
func (p *payloadStore) reserve(ctx context.Context, user int64, surface, device, lifetime, hash, purpose string) (payloadOwner, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if user <= 0 || !validPayloadID(hash) || lifetime == "" || purpose == "" {
		return payloadOwner{}, core.ErrNotAvailable
	}
	if surface == "" {
		surface = "plex"
	}
	if err := p.enter(ctx); err != nil {
		return payloadOwner{}, err
	}
	defer p.leave()
	if err := p.begin(); err != nil {
		return payloadOwner{}, err
	}
	if err := p.verifyProof(p.file, p.incarnation); err != nil {
		p.finish()
		return payloadOwner{}, err
	}
	if anchor := p.anchors[hash]; anchor != nil {
		if anchor.retiring {
			p.finish()
			return payloadOwner{}, core.ErrNotAvailable
		}
		anchor.refs++
		ref := anchor.owner
		ref.state = &payloadOwnerState{id: randomID(32), anchor: anchor}
		return ref, nil
	}

	release := true
	defer func() {
		if release {
			p.finish()
		}
	}()
	h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: payloadNamespace, ViewerID: user, Surface: surface, DeviceID: device}, Digest: randomID(32), ExpiresMillis: time.Now().Add(time.Hour).UnixMilli()}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return payloadOwner{}, err
	}
	defer tx.Rollback()
	if err := capability.AdmitHeadersInTx(ctx, p.db, tx, []capability.Header{h}, capability.Limits{PerScope: 256, PerShard: 4096}); err != nil {
		return payloadOwner{}, err
	}
	if err := payloadLock(ctx, p.db, tx, p.domain, hash); err != nil {
		return payloadOwner{}, err
	}
	var deleting int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_payload_deleting WHERE domain=? AND payload_hash=?`, p.domain, hash).Scan(&deleting); err != nil {
		return payloadOwner{}, err
	}
	if deleting != 0 {
		return payloadOwner{}, core.ErrNotAvailable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO playback_payload_owners(namespace,digest,viewer_id,surface,device_id,domain,incarnation,source_lifetime,payload_hash,purpose) VALUES(?,?,?,?,?,?,?,?,?,?)`, payloadNamespace, h.Digest, user, surface, device, p.domain, p.incarnation, lifetime, hash, purpose); err != nil {
		return payloadOwner{}, err
	}
	claimed, err := capability.ClaimHeaderInTx(ctx, p.db, tx, h, capability.RetirementOwnerClaim, time.Now().UnixMilli())
	if err != nil {
		return payloadOwner{}, err
	}
	if !claimed {
		return payloadOwner{}, core.ErrNotAvailable
	}
	if err := tx.Commit(); err != nil {
		release = false
		return payloadOwner{}, fmt.Errorf("%w: %w", errPayloadCommitUnknown, err)
	}
	if p.afterOwnerCommit != nil {
		if err := p.afterOwnerCommit(); err != nil {
			release = false
			return payloadOwner{}, fmt.Errorf("%w: %w", errPayloadCommitUnknown, err)
		}
	}
	release = false
	owner := payloadOwner{header: h, domain: p.domain, incarnation: p.incarnation, hash: hash}
	anchor := &payloadAnchor{owner: owner, refs: 1}
	owner.state = &payloadOwnerState{id: randomID(32), anchor: anchor}
	p.anchors[hash] = anchor
	return owner, nil
}
func dropPayloadOwner(ctx context.Context, db *sql.DB, tx *sql.Tx, o payloadOwner) (string, error) {
	if err := capability.LockHeaderScopeInTx(ctx, db, tx, o.header.HeaderScope); err != nil {
		return "", err
	}
	if err := store.LockKey(ctx, store.DialectOf(db), tx, "playback:payload-delete-admission"); err != nil {
		return "", err
	}
	if err := payloadLock(ctx, db, tx, o.domain, o.hash); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playback_payload_owners WHERE namespace=? AND digest=? AND domain=? AND incarnation=?`, payloadNamespace, o.header.Digest, o.domain, o.incarnation); err != nil {
		return "", err
	}
	if err := capability.DropHeadersInTx(ctx, tx, o.header.HeaderScope, []string{o.header.Digest}); err != nil {
		return "", err
	}
	return markPayloadDeletingTx(ctx, tx, o.domain, o.hash, o.incarnation)
}

func markPayloadDeletingTx(ctx context.Context, tx *sql.Tx, domain, hash, incarnation string) (string, error) {
	var owners, refs int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_payload_owners WHERE domain=? AND payload_hash=?`, domain, hash).Scan(&owners); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_resume WHERE nzb_hash=?`, hash).Scan(&refs); err != nil {
		return "", err
	}
	if owners != 0 || refs != 0 {
		return "", nil
	}
	var marker, who string
	err := tx.QueryRowContext(ctx, `SELECT digest,incarnation FROM playback_payload_deleting WHERE domain=? AND payload_hash=?`, domain, hash).Scan(&marker, &who)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err == nil {
		if who != incarnation {
			return "", nil
		}
		return marker, nil
	}
	var global, scoped int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN domain=? THEN 1 ELSE 0 END),0) FROM playback_payload_deleting`, domain).Scan(&global, &scoped); err != nil {
		return "", err
	}
	if global >= 8192 || scoped >= 256 {
		return "", core.ErrNotAvailable
	}
	marker = randomID(32)
	if _, err := tx.ExecContext(ctx, `INSERT INTO playback_payload_deleting(domain,payload_hash,digest,incarnation) VALUES(?,?,?,?)`, domain, hash, marker, incarnation); err != nil {
		return "", err
	}
	return marker, nil
}

func (p *payloadStore) release(ctx context.Context, o payloadOwner) error {
	if o.state == nil {
		return core.ErrNotAvailable
	}
	o.state.mu.Lock()
	defer o.state.mu.Unlock()
	if o.state.released {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := p.enter(ctx); err != nil {
		return err
	}
	defer p.leave()
	anchor := o.state.anchor
	if anchor == nil || p.anchors[o.hash] != anchor {
		return core.ErrNotAvailable
	}
	if anchor.refs > 1 {
		anchor.refs--
		o.state.released = true
		p.finish()
		return nil
	}
	anchor.retiring = true

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	marker, err := dropPayloadOwner(ctx, p.db, tx, o)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if marker != "" {
		if err := p.removeMarked(ctx, o.hash, o.incarnation, marker, p.file); err != nil {
			return err
		}
	}
	delete(p.anchors, o.hash)
	anchor.refs = 0
	o.state.released = true
	p.finish()
	return nil
}

// recoverIncarnation is called only while holding that exact, verified kernel
// witness, proving its real backend/writer can no longer be using a payload.
func (p *payloadStore) recoverIncarnation(ctx context.Context, incarnation string, f *os.File) (bool, error) {
	value, err := readWitness(f)
	if err != nil || value != p.domain+"\n"+incarnation {
		return false, err
	}
	if err := p.verifyProof(f, incarnation); err != nil {
		return false, err
	}
	var slot int
	err = p.db.QueryRowContext(ctx, `SELECT witness_slot FROM playback_payload_incarnations WHERE domain=? AND incarnation=?`, p.domain, incarnation).Scan(&slot)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && filepath.Base(f.Name()) != fmt.Sprintf("%03d.lock", slot) {
		return false, core.ErrNotAvailable
	}

	rows, err := p.db.QueryContext(ctx, `SELECT digest,viewer_id,surface,device_id,payload_hash FROM playback_payload_owners WHERE domain=? AND incarnation=? ORDER BY digest LIMIT ?`, p.domain, incarnation, payloadRecoveryBatch)
	if err != nil {
		return false, err
	}
	var owners []payloadOwner
	for rows.Next() {
		o := payloadOwner{domain: p.domain, incarnation: incarnation}
		o.header.Namespace = payloadNamespace
		if err := rows.Scan(&o.header.Digest, &o.header.ViewerID, &o.header.Surface, &o.header.DeviceID, &o.hash); err != nil {
			rows.Close()
			return false, err
		}
		owners = append(owners, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, o := range owners {
		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		marker, err := dropPayloadOwner(ctx, p.db, tx, o)
		if err != nil {
			tx.Rollback()
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		if marker != "" {
			if err := p.removeMarked(ctx, o.hash, incarnation, marker, f); err != nil {
				return false, err
			}
		}
	}
	markers, err := p.db.QueryContext(ctx, `SELECT payload_hash FROM playback_payload_deleting WHERE domain=? AND incarnation=? ORDER BY payload_hash LIMIT ?`, p.domain, incarnation, payloadRecoveryBatch)
	if err != nil {
		return false, err
	}
	var hashes []string
	for markers.Next() {
		var hash string
		if err := markers.Scan(&hash); err != nil {
			markers.Close()
			return false, err
		}
		hashes = append(hashes, hash)
	}
	err = markers.Err()
	markers.Close()
	if err != nil {
		return false, err
	}
	for _, hash := range hashes {
		if err := p.removeUnreferenced(ctx, hash, incarnation, f); err != nil {
			return false, err
		}
	}
	// RESTRICT is a hard backstop if either bounded batch has work remaining.
	var n int
	if err := p.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM playback_payload_owners WHERE domain=? AND incarnation=?)+(SELECT COUNT(*) FROM playback_payload_deleting WHERE domain=? AND incarnation=?)`, p.domain, incarnation, p.domain, incarnation).Scan(&n); err != nil {
		return false, err
	}
	if n != 0 {
		return false, nil
	}
	_, err = p.db.ExecContext(ctx, `DELETE FROM playback_payload_incarnations WHERE domain=? AND incarnation=?`, p.domain, incarnation)
	return err == nil, err
}

// removeUnreferenced commits a marker before filesystem removal. Admission and
// every resume writer take the same payload lock and reject the marker. A crash
// or unknown completion leaves it for the verified incarnation's reconciliation.
func (p *payloadStore) removeUnreferenced(ctx context.Context, hash, incarnation string, witness *os.File) error {
	if !validPayloadID(hash) {
		return core.ErrNotAvailable
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.LockKey(ctx, store.DialectOf(p.db), tx, "playback:payload-delete-admission"); err != nil {
		return err
	}
	if err := payloadLock(ctx, p.db, tx, p.domain, hash); err != nil {
		return err
	}
	marker, err := markPayloadDeletingTx(ctx, tx, p.domain, hash, incarnation)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if marker == "" {
		return nil
	}
	return p.removeMarked(ctx, hash, incarnation, marker, witness)
}
func (p *payloadStore) removeMarked(ctx context.Context, hash, incarnation, marker string, witness *os.File) error {
	if err := p.verifyProof(witness, incarnation); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(p.dir, hash+".nzb")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if p.afterRemove != nil {
		if err := p.afterRemove(); err != nil {
			return err
		}
	}
	_, err := p.db.ExecContext(ctx, `DELETE FROM playback_payload_deleting WHERE domain=? AND payload_hash=? AND digest=? AND incarnation=?`, p.domain, hash, marker, incarnation)
	return err
}

func (p *payloadStore) gc(ctx context.Context, hash string) error {
	if err := p.enter(ctx); err != nil {
		return err
	}
	defer p.leave()
	if err := p.begin(); err != nil {
		return err
	}
	if err := p.removeUnreferenced(ctx, hash, p.incarnation, p.file); err != nil {
		return err
	}
	p.finish()
	return nil
}

// queueRelease is bounded by the same admitted-owner budget as reserve. It is
// safe from an outbox merge callback: no filesystem/DB work occurs under mu.
func (p *payloadStore) queueRelease(o payloadOwner) {
	p.mu.Lock()
	if p.retired == nil {
		p.retired = make(map[string]payloadOwner)
	}
	p.retired[o.state.id] = o
	if !p.releaseRunning && !p.closing {
		p.releaseRunning = true
		go p.runReleases()
	}
	p.mu.Unlock()
}
func (p *payloadStore) runReleases() {
	defer func() { p.mu.Lock(); p.releaseRunning = false; p.mu.Unlock() }()
	for {
		p.mu.Lock()
		if p.closing || len(p.retired) == 0 {
			p.mu.Unlock()
			return
		}
		var id string
		var owner payloadOwner
		for key, value := range p.retired {
			id, owner = key, value
			break
		}
		p.mu.Unlock()
		if err := p.release(context.Background(), owner); err == nil {
			p.mu.Lock()
			delete(p.retired, id)
			p.mu.Unlock()
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-p.releaseStop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *payloadStore) enter(ctx context.Context) error {
	select {
	case p.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *payloadStore) leave() { <-p.gate }

func (p *payloadStore) verifyProof(f *os.File, incarnation string) error {
	reliable, err := payloadFilesystemReliable(p.dir)
	if err != nil {
		return err
	}
	if !reliable {
		return errPayloadFilesystem
	}
	if f == nil {
		return core.ErrNotAvailable
	}
	if err := verifyWitnessFile(f); err != nil {
		return err
	}
	value, err := readWitness(f)
	if err != nil {
		return err
	}
	if value != p.domain+"\n"+incarnation {
		return core.ErrNotAvailable
	}
	domain, err := os.ReadFile(filepath.Join(p.dir, ".domain"))
	if err != nil {
		return err
	}
	if string(domain) != p.domain {
		return core.ErrNotAvailable
	}
	return nil
}

// scheduleRecovery reuses the manager's pin-maintenance trigger. Only one
// bounded actor can reconcile this store; no broker gate waits for filesystem
// or DB work, and shutdown cancellation never drops a witness mid-operation.
func (p *payloadStore) scheduleRecovery() {
	p.mu.Lock()
	if p.closing || p.recoveryRunning || p.active >= 4096 {
		p.mu.Unlock()
		return
	}
	p.recoveryRunning = true
	p.active++
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	p.recoveryCancel = cancel
	p.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			p.mu.Lock()
			p.recoveryRunning = false
			p.recoveryCancel = nil
			p.mu.Unlock()
			p.finish()
		}()
		p.reconcileSlots(ctx)
	}()
}
func (p *payloadStore) reconcileSlots(ctx context.Context) error {
	reliable, err := payloadFilesystemReliable(p.dir)
	if err != nil {
		return err
	}
	if !reliable {
		return errPayloadFilesystem
	}
	p.mu.Lock()
	first := p.cursor
	p.cursor = (p.cursor + 4) % payloadWitnessSlots
	p.mu.Unlock()
	for i := 0; i < 4; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		slot := (first + i) % payloadWitnessSlots
		f, err := os.OpenFile(filepath.Join(p.dir, ".owners", fmt.Sprintf("%03d.lock", slot)), os.O_RDWR, 0600)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if err := lockPayloadWitness(f); err != nil {
			f.Close()
			continue
		}
		value, err := readWitness(f)
		if err != nil {
			f.Close()
			return err
		}
		parts := strings.Split(value, "\n")
		if len(parts) != 2 || parts[0] != p.domain || !validPayloadID(parts[1]) || parts[1] == p.incarnation {
			f.Close()
			continue
		}
		_, err = p.recoverIncarnation(ctx, parts[1], f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func (p *payloadStore) pendingError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active != 0 {
		return fmt.Errorf("playback: %d payload ownership operations remain protected", p.active)
	}
	return nil
}

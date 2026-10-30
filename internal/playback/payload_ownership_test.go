package playback

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func payloadFixture(t *testing.T) *payloadStore {
	t.Helper()
	db := store.OpenTest(t)
	dir := t.TempDir()
	p, err := openPayloadStore(context.Background(), db, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	return p
}
func payloadReserve(t *testing.T, p *payloadStore, hash string) payloadOwner {
	t.Helper()
	o, err := p.reserve(context.Background(), 1, "plex", "tv", "lifetime", hash, "source-open")
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func payloadRows(t *testing.T, p *payloadStore, table, incarnation string) int {
	t.Helper()
	var n int
	if err := p.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table+` WHERE domain=? AND incarnation=?`, p.domain, incarnation).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestPayloadSharedAnchorAcquireAndLastRelease(t *testing.T) {
	p := payloadFixture(t)
	ctx := context.Background()
	hash := nzbHash([]byte("shared"))
	first := payloadReserve(t, p, hash)
	const count = 16
	refs := make(chan payloadOwner, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := p.reserve(ctx, 2, "silo", "other-device", "other-lifetime", hash, "resume-write")
			refs <- o
			errs <- err
		}()
	}
	wg.Wait()
	close(refs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := payloadRows(t, p, "playback_payload_owners", p.incarnation); n != 1 {
		t.Fatalf("payload anchors=%d want1", n)
	}
	if err := p.release(ctx, first); err != nil {
		t.Fatal(err)
	}
	for o := range refs {
		if o.header.Digest != first.header.Digest {
			t.Fatal("exact payload did not share committed anchor")
		}
		if err := p.release(ctx, o); err != nil {
			t.Fatal(err)
		}
		if err := p.release(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if n := payloadRows(t, p, "playback_payload_owners", p.incarnation); n != 0 {
		t.Fatalf("retired anchor=%d", n)
	}
	replacement := payloadReserve(t, p, hash)
	if replacement.header.Digest == first.header.Digest {
		t.Fatal("retired anchor resurrected")
	}
	if err := p.release(ctx, first); err != nil {
		t.Fatal(err)
	}
	if n := payloadRows(t, p, "playback_payload_owners", p.incarnation); n != 1 {
		t.Fatal("duplicate old release retired replacement")
	}
	if err := p.release(ctx, replacement); err != nil {
		t.Fatal(err)
	}
}
func TestPayloadAcquireVersusRetireDoesNotResurrect(t *testing.T) {
	p := payloadFixture(t)
	ctx := context.Background()
	hash := nzbHash([]byte("race"))
	for i := 0; i < 20; i++ {
		first := payloadReserve(t, p, hash)
		start := make(chan struct{})
		acquired := make(chan payloadOwner, 1)
		errs := make(chan error, 2)
		go func() { <-start; errs <- p.release(ctx, first) }()
		go func() {
			<-start
			o, err := p.reserve(ctx, 1, "plex", "tv", "replacement", hash, "source-open")
			acquired <- o
			errs <- err
		}()
		close(start)
		a, b := <-errs, <-errs
		o := <-acquired
		if a != nil || b != nil {
			t.Fatalf("acquire/retire: %v %v", a, b)
		}
		if n := payloadRows(t, p, "playback_payload_owners", p.incarnation); n != 1 {
			t.Fatalf("live anchor count=%d", n)
		}
		if err := p.release(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
}
func TestPayloadHeaderPruningKeepsActualOwner(t *testing.T) {
	p := payloadFixture(t)
	ctx := context.Background()
	o := payloadReserve(t, p, nzbHash([]byte("owned")))
	var free capability.Header
	free = o.header
	free.Digest = randomID(32)
	free.ExpiresMillis = time.Now().Add(-time.Hour).UnixMilli()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := capability.BindHeadersInTx(ctx, tx, []capability.Header{free}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE capability_headers SET expires=1 WHERE namespace=? AND digest=?`, payloadNamespace, o.header.Digest); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = p.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	n, err := capability.PruneHeadersInTx(ctx, tx, payloadNamespace, time.Now().UnixMilli(), 1)
	if err != nil || n != 1 {
		t.Fatalf("housekeeping did not progress: %d %v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := payloadRows(t, p, "playback_payload_owners", p.incarnation); n != 1 {
		t.Fatal("housekeeping retired live source")
	}
	if _, err := p.db.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND digest=?`, payloadNamespace, o.header.Digest); err == nil {
		t.Fatal("RESTRICT backstop accepted direct deletion")
	}
	if err := p.release(ctx, o); err != nil {
		t.Fatal(err)
	}
}
func TestPayloadCrashCommitAndDeleteRecovery(t *testing.T) {
	for _, stage := range []string{"commit", "delete"} {
		t.Run(stage, func(t *testing.T) {
			p := payloadFixture(t)
			ctx := context.Background()
			hash := nzbHash([]byte("crash"))
			path := filepath.Join(p.dir, hash+".nzb")
			if err := os.WriteFile(path, []byte("crash"), 0600); err != nil {
				t.Fatal(err)
			}
			unknown := errors.New("lost completion acknowledgement")
			if stage == "commit" {
				p.afterOwnerCommit = func() error { return unknown }
				if _, err := p.reserve(ctx, 1, "plex", "tv", "lost", hash, "source-open"); !errors.Is(err, errPayloadCommitUnknown) {
					t.Fatalf("commit uncertainty=%v", err)
				}
			} else {
				o := payloadReserve(t, p, hash)
				p.afterRemove = func() error { return unknown }
				if err := p.release(ctx, o); !errors.Is(err, unknown) {
					t.Fatalf("delete uncertainty=%v", err)
				}
				if n := payloadRows(t, p, "playback_payload_deleting", p.incarnation); n != 1 {
					t.Fatal("uncertain delete lost marker")
				}
			}
			// A restarted manager cannot reclaim a live witness, including a stalled
			// or uncertain operation in another manager in this same process.
			live, err := openPayloadStore(ctx, p.db, p.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer live.close()
			if stage == "commit" && payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
				t.Fatal("live incarnation reclaimed")
			}
			// Model process death after the persisted DB/file boundary: no old code
			// will run again, and the kernel releases its exact witness description.
			if err := p.file.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := openPayloadStore(ctx, p.db, p.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.close()
			if payloadRows(t, p, "playback_payload_owners", p.incarnation) != 0 || payloadRows(t, p, "playback_payload_deleting", p.incarnation) != 0 {
				t.Fatal("dead incarnation leaked durable ownership")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("dead unreferenced payload remains: %v", err)
			}
			if _, err := p.reserve(ctx, 1, "plex", "tv", "stale", hash, "source-open"); err == nil {
				t.Fatal("retired incarnation resurrected")
			}
		})
	}
}
func TestPayloadMissingOrUnverifiableWitnessFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "changed-content"} {
		t.Run(mode, func(t *testing.T) {
			p := payloadFixture(t)
			hash := nzbHash([]byte("protected"))
			payloadReserve(t, p, hash)
			path := p.file.Name()
			if err := p.file.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("unverifiable"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := openPayloadStore(context.Background(), p.db, p.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.close()
			if payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
				t.Fatal("uncertain witness authorized reclamation")
			}
		})
	}
}
func TestPayloadAllWitnessSlotsHeldRefusesManager(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	p := h.m.resume.payload
	for i := 1; i < payloadWitnessSlots; i++ {
		other, err := openPayloadStore(context.Background(), p.db, p.dir)
		if err != nil {
			t.Fatal(err)
		}
		defer other.close()
	}
	m, err := New(h.m.deps, h.cfg, h.m.opts)
	if m != nil {
		m.Close()
		t.Fatal("new manager evicted a witness")
	}
	if !errors.Is(err, errPayloadWitnessCapacity) {
		t.Fatalf("full witness error=%v", err)
	}
	if err := p.verifyProof(p.file, p.incarnation); err != nil {
		t.Fatal("existing manager lost witness", err)
	}
}
func TestPayloadWitnessProcessHelper(t *testing.T) {
	path := os.Getenv("ETHER_PAYLOAD_WITNESS_HELPER")
	if path == "" {
		t.Skip("subprocess helper")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockPayloadWitness(f); err != nil {
		t.Fatal(err)
	}
	fmt.Println("WITNESS_HELD")
	var b [1]byte
	os.Stdin.Read(b[:])
	os.Exit(0) // deliberately bypass Close: the kernel, not a callback, releases it
}
func TestPayloadDeadProcessReleasesWitness(t *testing.T) {
	p := payloadFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hash := nzbHash([]byte("child"))
	payloadReserve(t, p, hash)
	path := p.file.Name()
	p.file.Close()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPayloadWitnessProcessHelper$")
	cmd.Env = append(os.Environ(), "ETHER_PAYLOAD_WITNESS_HELPER="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		input.Close()
		if !waited {
			cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "WITNESS_HELD" {
		t.Fatalf("child witness=%q %v", line, err)
	}
	live, err := openPayloadStore(ctx, p.db, p.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer live.close()
	if payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
		t.Fatal("live process owner reclaimed")
	}
	input.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
	restarted, err := openPayloadStore(ctx, p.db, p.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	if payloadRows(t, p, "playback_payload_owners", p.incarnation) != 0 {
		t.Fatal("dead process ownership not reconciled")
	}
}
func TestPayloadStalledBackendCloseKeepsWitness(t *testing.T) {
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	e := &ownedTestEngine{Engine: h.engine, closeEntered: make(chan struct{}), closeRelease: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(e.closeRelease) }) }
	defer unblock()
	h.m.deps.Engine = e
	ps := h.prepare(owner, movieKey, "held-witness")
	outcome, _ := h.m.SourceRetirement(ps.ID)
	if err := h.m.RetireSource(ps.ID, outcome); err != nil {
		t.Fatal(err)
	}
	waitRetirementTest(t, e.closeEntered, "stalled backend Close")
	p := h.m.resume.payload
	other, err := openPayloadStore(context.Background(), p.db, p.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	if payloadRows(t, p, "playback_payload_owners", p.incarnation) != 1 {
		t.Fatal("stalled backend guard reclaimed")
	}
	requireOwnedPending(t, outcome)
	unblock()
	waitRetirementTest(t, outcome.Done(), "backend retirement")
}

func TestPayloadBoundedRecoveryFinishesRemainingOwners(t *testing.T) {
	p := payloadFixture(t)
	ctx := context.Background()
	for i := 0; i < payloadRecoveryBatch+5; i++ {
		payloadReserve(t, p, nzbHash([]byte(fmt.Sprintf("orphan-%d", i))))
	}
	p.file.Close()
	restarted, err := openPayloadStore(ctx, p.db, p.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	if remaining := payloadRows(t, p, "playback_payload_owners", p.incarnation); remaining != 5 {
		t.Fatalf("startup recovery batch left%d want5", remaining)
	}
	if err := restarted.reconcileSlots(ctx); err != nil {
		t.Fatal(err)
	}
	if remaining := payloadRows(t, p, "playback_payload_owners", p.incarnation); remaining != 0 {
		t.Fatalf("bounded sweep left%d", remaining)
	}
}
func TestPayloadReleasedWriterCannotReplayIntoSharedAnchor(t *testing.T) {
	rs := newTestResumeStore(t)
	ctx := context.Background()
	nzb := []byte("shared writer")
	rec := resumeRecord{userID: 1, key: movieKey, release: core.Release{ID: "A"}, updatedAt: time.Now()}
	one, err := rs.acquireNZB(ctx, rec, nzb)
	if err != nil {
		t.Fatal(err)
	}
	two, err := rs.acquireNZB(ctx, rec, nzb)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.releaseNZB(one); err != nil {
		t.Fatal(err)
	}
	if err := rs.writeBundle(ctx, rec, one, nil, core.MediaInfo{}, nzb); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("released writer replay=%v", err)
	}
	if err := rs.writeBundle(ctx, rec, two, nil, core.MediaInfo{}, nzb); err != nil {
		t.Fatal(err)
	}
	if err := rs.releaseNZB(two); err != nil {
		t.Fatal(err)
	}
}

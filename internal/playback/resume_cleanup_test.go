package playback

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A real DELETE failure after the bundle commits must only retain cleanup
// responsibility. It must not enqueue the committed writer for another Apply.
func TestResumeCommittedWriteSurvivesCleanupFailure(t *testing.T) {
	for _, mode := range []string{"save", "put", "outbox-direct", "outbox-replay"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db := store.OpenTest(t)
			rs, err := newResumeStore(db, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rs.close(ctx) })
			repair := blockResumeOwnerCleanup(t, db)
			t.Cleanup(repair)
			rec := resumeRecord{userID: 1, key: movieKey, release: core.Release{ID: "A"}, updatedAt: time.Now()}
			nzb := []byte("committed A")
			var avail *store.Availability
			switch mode {
			case "save":
				err = rs.save(ctx, rec, nzb)
			case "put":
				err = rs.put(ctx, rec, nzb, nil)
			default:
				avail = store.NewAvailability(nil)
				rs.hold(avail, slog.New(slog.NewTextHandler(io.Discard, nil)))
				lease, acquireErr := rs.acquireNZB(ctx, rec, nzb)
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				replay := mode == "outbox-replay"
				if replay {
					avail.Fail(errors.New("test outage before Apply"))
				}
				queued, writeErr := rs.held.Write(ctx, resumeKey{rec.userID, rec.key}, resumeWrite{rec: rec, lease: lease, nzb: nzb})
				err = writeErr
				if queued != replay {
					t.Fatalf("queued=%v want %v: committed write treated as failed", queued, replay)
				}
				if replay {
					avail.OK()
				}
				deadline := time.Now().Add(5 * time.Second)
				for rs.held.Depth() != 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if rs.held.Depth() != 0 || avail.Down() {
					t.Fatalf("committed Apply affected outbox/availability: %+v %+v", rs.held.Status(), avail.State())
				}
			}
			if err != nil {
				t.Fatalf("committed bundle returned cleanup error: %v", err)
			}
			assertResumeRelease(t, db, "A")
			rs.payload.mu.Lock()
			pending, active := len(rs.payload.retired), rs.payload.active
			var owner payloadOwner
			for _, o := range rs.payload.retired {
				owner = o
			}
			rs.payload.mu.Unlock()
			if pending != 1 || active != 1 || owner.state == nil {
				t.Fatalf("cleanup responsibility lost: queued=%d active=%d", pending, active)
			}
			if n := payloadRows(t, rs.payload, "playback_payload_owners", rs.payload.incarnation); n != 1 {
				t.Fatalf("failed cleanup lost durable owner: %d", n)
			}
			repair()
			deadline := time.Now().Add(5 * time.Second)
			for {
				rs.payload.mu.Lock()
				drained := len(rs.payload.retired) == 0 && rs.payload.active == 0 && !rs.payload.releaseRunning
				rs.payload.mu.Unlock()
				if drained {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("exact cleanup did not finish after repair")
				}
				time.Sleep(time.Millisecond)
			}
			// The old lease is truly retired before the next write. It cannot
			// be replayed to overwrite B, and duplicate cleanup is harmless.
			if err := rs.payload.release(ctx, owner); err != nil {
				t.Fatal(err)
			}
			rec.release.ID = "B"
			if err := rs.put(ctx, rec, []byte("newer B"), nil); err != nil {
				t.Fatal(err)
			}
			if rs.held != nil {
				if err := rs.held.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertResumeRelease(t, db, "B")
			if n := payloadRows(t, rs.payload, "playback_payload_owners", rs.payload.incarnation); n != 0 {
				t.Fatalf("cleanup left durable owners: %d", n)
			}
		})
	}
}

func assertResumeRelease(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`SELECT release_id FROM playback_resume WHERE user_id=? AND rating_key=?`, 1, movieKey).Scan(&got); err != nil || got != want {
		t.Fatalf("committed release=%q want %q: %v", got, want, err)
	}
}

func blockResumeOwnerCleanup(t *testing.T, db *sql.DB) func() {
	t.Helper()
	ddl := `CREATE TRIGGER fail_resume_cleanup BEFORE DELETE ON playback_payload_owners BEGIN SELECT RAISE(ABORT, 'injected cleanup unexpected EOF'); END`
	drop := `DROP TRIGGER fail_resume_cleanup`
	if store.DialectOf(db) == store.Postgres {
		if _, err := db.Exec(`CREATE FUNCTION fail_resume_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected cleanup failure' USING ERRCODE='08006'; END $$`); err != nil {
			t.Fatal(err)
		}
		ddl = `CREATE TRIGGER fail_resume_cleanup BEFORE DELETE ON playback_payload_owners FOR EACH ROW EXECUTE FUNCTION fail_resume_cleanup()`
		drop = `DROP TRIGGER fail_resume_cleanup ON playback_payload_owners`
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	repaired := false
	return func() {
		if !repaired {
			if _, err := db.Exec(drop); err != nil {
				t.Fatal(err)
			}
			repaired = true
		}
	}
}

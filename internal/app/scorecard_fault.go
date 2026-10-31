//go:build scorecard && !plexparity

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/braedonsaunders/ether/internal/hoststat"
)

type scorecardFaultState struct {
	mu    sync.Mutex
	mode  string
	delay time.Duration
	until time.Time
}

var scorecardFaults scorecardFaultState

func scorecardFaultClear() {
	scorecardFaults.mu.Lock()
	scorecardFaults.mode = ""
	scorecardFaults.delay = 0
	scorecardFaults.until = time.Time{}
	scorecardFaults.mu.Unlock()
}

func scorecardFaultBeforeIO() error {
	scorecardFaults.mu.Lock()
	mode, delay, until := scorecardFaults.mode, scorecardFaults.delay, scorecardFaults.until
	if !until.IsZero() && time.Now().After(until) {
		scorecardFaults.mode = ""
		scorecardFaults.delay = 0
		scorecardFaults.until = time.Time{}
		mode = ""
	}
	scorecardFaults.mu.Unlock()
	switch mode {
	case "outage":
		return errors.New("scorecard source outage")
	case "slow":
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	return nil
}

func (m *scorecardMedia) Read(p []byte) (int, error) {
	if err := scorecardFaultBeforeIO(); err != nil {
		return 0, err
	}
	return m.File.Read(p)
}

func (m *scorecardMedia) ReadAt(p []byte, off int64) (int, error) {
	if err := scorecardFaultBeforeIO(); err != nil {
		return 0, err
	}
	return m.File.ReadAt(p, off)
}

func scorecardTelemetry(a *App) hoststat.Telemetry {
	sessions, transcodes := 0, 0
	if a != nil && a.playback != nil {
		sessions = len(a.playback.Sessions())
	}
	if a != nil && a.transcoder != nil {
		transcodes = len(a.transcoder.Sessions())
	}
	return hoststat.Collect("/proc", "/dev", os.Getpid(), sessions, transcodes)
}

func scorecardSignal(mode string) (int, error) {
	tree, err := hoststat.ProcessTree("/proc", os.Getpid())
	if err != nil {
		return 0, err
	}
	pids := tree.FFmpegPIDs
	if mode == "guardian" {
		pids = tree.GuardianPIDs
	}
	signaled := 0
	for _, pid := range pids {
		if pid <= 1 || pid == os.Getpid() {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return signaled, err
		}
		signaled++
	}
	return signaled, nil
}

func scorecardStallDB(db *sql.DB, d time.Duration) error {
	if db == nil {
		return errors.New("no database")
	}
	if d <= 0 || d > 15*time.Second {
		return errors.New("db stall duration out of range")
	}
	ctx, cancel := context.WithTimeout(context.Background(), d+20*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	_, err = conn.ExecContext(context.Background(), "ROLLBACK")
	return err
}

// scorecardControl serves loopback soak telemetry and fault injection.
// The caller has already rejected non-loopback clients.
func scorecardControl(a *App, w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/__scorecard/telemetry":
		if r.Method != http.MethodGet {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(scorecardTelemetry(a))
		return true
	case "/__scorecard/fault":
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return true
		}
		var body struct {
			Action     string `json:"action"`
			DelayMS    int    `json:"delay_ms"`
			DurationMS int    `json:"duration_ms"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, "bad fault", http.StatusBadRequest)
			return true
		}
		signaled, err := applyScorecardFault(a, body.Action, body.DelayMS, body.DurationMS)
		if err != nil {
			http.Error(w, "fault rejected", http.StatusConflict)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "signaled": signaled})
		return true
	default:
		return false
	}
}

func applyScorecardFault(a *App, action string, delayMS, durationMS int) (int, error) {
	if delayMS < 0 || delayMS > 2000 || durationMS < 0 || durationMS > 120000 {
		return 0, errors.New("fault timing out of range")
	}
	hold := time.Duration(durationMS) * time.Millisecond
	switch action {
	case "clear":
		scorecardFaultClear()
		return 0, nil
	case "source_outage", "slow_upstream":
		if hold <= 0 {
			hold = 30 * time.Second
		}
		scorecardFaults.mu.Lock()
		if action == "source_outage" {
			scorecardFaults.mode = "outage"
			scorecardFaults.delay = 0
		} else {
			scorecardFaults.mode = "slow"
			scorecardFaults.delay = time.Duration(delayMS) * time.Millisecond
		}
		scorecardFaults.until = time.Now().Add(hold)
		scorecardFaults.mu.Unlock()
		return 0, nil
	case "encoder_crash":
		return scorecardSignal("ffmpeg")
	case "encoder_kill":
		return scorecardSignal("guardian")
	case "db_stall":
		if a == nil {
			return 0, errors.New("no database")
		}
		return 0, scorecardStallDB(a.db, hold)
	default:
		return 0, errors.New("unknown fault")
	}
}

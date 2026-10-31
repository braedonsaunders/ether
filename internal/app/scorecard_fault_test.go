//go:build scorecard && !plexparity

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/hoststat"
)

func TestScorecardFaultOutageAndTelemetry(t *testing.T) {
	t.Cleanup(scorecardFaultClear)
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mp4")
	payload := []byte("fixture-bytes")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	raw, _ := json.Marshal([]ScorecardFile{{TMDB: 1, Path: path, SHA256: hex.EncodeToString(sum[:])}})
	if err := os.WriteFile(filepath.Join(dir, "scorecard-files.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/__scorecard/fault", strings.NewReader(`{"action":"source_outage","duration_ms":2000}`))
	if !scorecardControl(&App{}, recorder, request) || recorder.Code != http.StatusOK {
		t.Fatalf("outage %d %s", recorder.Code, recorder.Body.String())
	}
	id := scorecardReleaseID(ScorecardFile{TMDB: 1, Path: path, SHA256: hex.EncodeToString(sum[:])})
	if _, err := (scorecardSource{dir}).Open(context.Background(), id, []byte(id)); err == nil {
		t.Fatal("open during outage")
	}
	scorecardFaultClear()
	opened, err := (scorecardSource{dir}).Open(context.Background(), id, []byte(id))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	media := opened.(*scorecardMedia)
	scorecardFaults.mu.Lock()
	scorecardFaults.mode = "slow"
	scorecardFaults.delay = 30 * time.Millisecond
	scorecardFaults.until = time.Now().Add(time.Second)
	scorecardFaults.mu.Unlock()
	started := time.Now()
	buf := make([]byte, 4)
	if _, err := media.Read(buf); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 25*time.Millisecond {
		t.Fatal("slow read did not delay")
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/__scorecard/telemetry", nil)
	if !scorecardControl(&App{}, recorder, request) || recorder.Code != 200 {
		t.Fatalf("telemetry %d", recorder.Code)
	}
	var sample hoststat.Telemetry
	if err := json.Unmarshal(recorder.Body.Bytes(), &sample); err != nil || sample.Goroutines < 1 || sample.GPU == "" {
		t.Fatalf("sample %+v %v", sample, err)
	}
}

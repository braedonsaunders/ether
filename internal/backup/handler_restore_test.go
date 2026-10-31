package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

type restartCall struct {
	reason   string
	whenIdle bool
}

// guidedServer is a standalone server with one snapshot, a recording
// restart and a real port in its settings.
func guidedServer(t *testing.T) (*Service, string, *[]restartCall) {
	t.Helper()
	s := standalone(t)
	if err := os.WriteFile(s.configPath, []byte(`{"server":{"port":32455,"bindAddress":"10.0.0.5"},"plex":{"token":"private"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sn, err := s.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var calls []restartCall
	s.SetRestart(func(reason string, whenIdle bool) error {
		calls = append(calls, restartCall{reason, whenIdle})
		return nil
	})
	return s, sn.Name, &calls
}

func decodeStaged(t *testing.T, body []byte) Staged {
	t.Helper()
	var st Staged
	if err := json.Unmarshal(body, &st); err != nil || st.ID == "" {
		t.Fatalf("staged response %s: %v", body, err)
	}
	return st
}

func TestGuidedRestoreFromASnapshot(t *testing.T) {
	s, name, calls := guidedServer(t)
	dataDir := filepath.Dir(s.configPath)
	h, _ := backupAPI(t, s, true)

	if w := (call{method: "POST", path: "/restore/snapshot/" + name}).do(h); w.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless stage: %d", w.Code)
	}
	if w := (call{method: "POST", path: "/restore/snapshot/../config.json", owner: true}).do(h); w.Code == 200 {
		t.Fatal("staged a path outside the backups")
	}
	w := (call{method: "POST", path: "/restore/snapshot/" + name, owner: true}).do(h)
	if w.Code != 200 {
		t.Fatalf("stage: %d %s", w.Code, w.Body)
	}
	st := decodeStaged(t, w.Body.Bytes())
	if st.Kind != "snapshot" || st.Source != name || st.Created.IsZero() || st.Logical || st.Size == 0 {
		t.Fatalf("staged = %+v", st)
	}
	if len(*calls) != 0 {
		t.Fatal("staging restarted the server")
	}
	// The owner changes the port after the backup; the restore must keep
	// listening where the console is.
	if err := os.WriteFile(s.configPath, []byte(`{"server":{"port":32460,"bindAddress":"10.0.0.9"},"plex":{"token":"private"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w = (call{method: "POST", path: "/restore/apply/" + st.ID, owner: true, body: []byte(`{"whenIdle":true}`)}).do(h)
	if w.Code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	if len(*calls) != 1 || !(*calls)[0].whenIdle {
		t.Fatalf("restart calls = %+v", *calls)
	}
	p, err := ReadPending(dataDir)
	if err != nil || p == nil || p.Staged != st.ID || p.Source != name || p.Kind != "snapshot" {
		t.Fatalf("pending = %+v, %v", p, err)
	}
	staged, err := config.Read(filepath.Join(dataDir, st.ID))
	if err != nil {
		t.Fatal(err)
	}
	if staged.Server.Port != 32460 || staged.Server.BindAddress != "10.0.0.9" || staged.Plex.Token != "private" {
		t.Fatalf("prepared config server=%+v token=%q", staged.Server, staged.Plex.Token)
	}
	// GET reports what is waiting.
	w = (call{method: "GET", path: "/restore", owner: true}).do(h)
	var status struct {
		OneClick bool     `json:"oneClick"`
		Pending  *Pending `json:"pending"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || !status.OneClick || status.Pending == nil {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}

	// The next start swaps it in; the report and undo follow. (This
	// fixture's database is in memory: give the data directory a file.)
	if err := os.WriteFile(filepath.Join(dataDir, "ether.db"), []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := ApplyPending(dataDir, time.Now)
	if err != nil || rep.Status != "applied" {
		t.Fatalf("apply at start = %+v, %v", rep, err)
	}
	if err := ConfirmRestore(dataDir, rep); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s.configPath); !strings.Contains(string(got), "ether-sealed:") && !strings.Contains(string(got), "32460") {
		t.Fatalf("restored config = %s", got)
	}
	w = (call{method: "GET", path: "/restore", owner: true}).do(h)
	if !strings.Contains(w.Body.String(), `"status":"restored"`) {
		t.Fatalf("report not served: %s", w.Body)
	}
	if w := (call{method: "POST", path: "/restore/undo", owner: true}).do(h); w.Code != http.StatusAccepted {
		t.Fatalf("undo: %d %s", w.Code, w.Body)
	}
	if p, _ := ReadPending(dataDir); p == nil || p.Kind != "undo" {
		t.Fatalf("undo pending = %+v", p)
	}
	if w := (call{method: "DELETE", path: "/restore/report", owner: true}).do(h); w.Code != 200 {
		t.Fatalf("dismiss: %d %s", w.Code, w.Body)
	}
	if r, _ := ReadReport(dataDir); r != nil {
		t.Fatal("report survived dismiss")
	}
}

func TestGuidedRestoreDiscardAndFailures(t *testing.T) {
	s, name, calls := guidedServer(t)
	dataDir := filepath.Dir(s.configPath)
	h, _ := backupAPI(t, s, true)
	st := decodeStaged(t, (call{method: "POST", path: "/restore/snapshot/" + name, owner: true}).do(h).Body.Bytes())
	if w := (call{method: "DELETE", path: "/restore/staged/" + st.ID, owner: true}).do(h); w.Code != 200 {
		t.Fatalf("discard: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(dataDir, st.ID)); !os.IsNotExist(err) {
		t.Fatal("discarded staging folder still present")
	}
	if w := (call{method: "POST", path: "/restore/apply/" + st.ID, owner: true}).do(h); w.Code != 400 {
		t.Fatalf("apply discarded: %d %s", w.Code, w.Body)
	}
	if w := (call{method: "POST", path: "/restore/apply/..%2Fconfig.json", owner: true}).do(h); w.Code == http.StatusAccepted {
		t.Fatal("applied a path outside the data directory")
	}
	if len(*calls) != 0 {
		t.Fatalf("restarted for an invalid request: %+v", *calls)
	}
	// A restart that cannot happen leaves nothing pending.
	st = decodeStaged(t, (call{method: "POST", path: "/restore/snapshot/" + name, owner: true}).do(h).Body.Bytes())
	s.SetRestart(func(string, bool) error { return os.ErrPermission })
	if w := (call{method: "POST", path: "/restore/apply/" + st.ID, owner: true}).do(h); w.Code != http.StatusConflict {
		t.Fatalf("apply with failing restart: %d %s", w.Code, w.Body)
	}
	if p, _ := ReadPending(dataDir); p != nil {
		t.Fatal("restore left pending after the restart failed")
	}
}

func TestGuidedRestoreIsGuidedOnACluster(t *testing.T) {
	s, name, calls := guidedServer(t)
	s.SetCluster(Node{ID: "n1", Name: "den"})
	h, _ := backupAPI(t, s, true)
	st := decodeStaged(t, (call{method: "POST", path: "/restore/snapshot/" + name, owner: true}).do(h).Body.Bytes())
	if st.Path == "" {
		t.Fatal("a cluster node needs the staged path for the guided restore")
	}
	w := (call{method: "POST", path: "/restore/apply/" + st.ID, owner: true}).do(h)
	if w.Code != http.StatusConflict || errCode(w) != "guided_restore" {
		t.Fatalf("cluster apply: %d %s", w.Code, w.Body)
	}
	if len(*calls) != 0 {
		t.Fatal("a cluster node restarted into a restore")
	}
	if p, _ := ReadPending(filepath.Dir(s.configPath)); p != nil {
		t.Fatal("a cluster node recorded a pending restore")
	}
}

// A cluster's bundle restored onto a standalone server must not point the
// server at the cluster's shared database.
func TestPrepareStagedClearsTheDatabaseSetting(t *testing.T) {
	dataDir := t.TempDir()
	id := StagedPrefix + "pg"
	writeFiles(t, filepath.Join(dataDir, id), map[string]string{
		"config.json":   `{"server":{"port":32400},"database":{"driver":"postgres","url":"postgres://u:p@db.lan/ether","deferMigrations":true}}`,
		LogicalDumpFile: "{}",
		"manifest.json": `{"version":1,"dialect":"postgres","sha256":{}}`,
	})
	if _, err := describeStaged(dataDir, id, "upload", "cluster.etherbackup"); err != nil {
		t.Fatal(err)
	}
	if err := PrepareStaged(dataDir, id, config.Server{Port: 32450}); err != nil {
		t.Fatal(err)
	}
	c, err := config.Read(filepath.Join(dataDir, id))
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.URL != "" || c.Database.Effective(nil).IsPostgres() || c.Database.DeferMigrations || c.Server.Port != 32450 {
		t.Fatalf("prepared database=%+v port=%d", c.Database, c.Server.Port)
	}
	if st, _ := StagedInfo(dataDir, id); !st.Logical {
		t.Fatalf("staged = %+v; want logical", st)
	}
}

func TestUploadNameIsDecodedAndBounded(t *testing.T) {
	for raw, want := range map[string]string{
		"ether%20backup%20%C3%A9.etherbackup": "ether backup é.etherbackup",
		"..%2F..%2Fetc%2Fpasswd":              "passwd",
		"":                                    "",
		"line%0Abreak":                        "",
	} {
		r, _ := http.NewRequest("POST", "/", nil)
		r.Header.Set("X-Ether-Backup-Name", raw)
		if got := uploadName(r); got != want {
			t.Errorf("uploadName(%q) = %q, want %q", raw, got, want)
		}
	}
}

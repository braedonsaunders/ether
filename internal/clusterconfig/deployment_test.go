package clusterconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestDeploymentAndDrainBoundary(t *testing.T) {
	dir := t.TempDir()
	// Secret paths are absolute for the host OS: "/secret/db" is only
	// drive-relative on Windows, so derive them from the absolute temp dir.
	secret := filepath.Join(dir, "secret")
	d := Deployment{Roles: config.NodeRoles{config.RoleTranscode}, InternalAddress: "https://node.internal:32401", DatabaseURLFile: filepath.Join(secret, "db"), ClusterKeyFile: filepath.Join(secret, "key"), CAFile: filepath.Join(secret, "issuer.crt"), CAKeyFile: filepath.Join(secret, "issuer.key"), RootsFile: filepath.Join(secret, "roots"), Capacity: 2, ClockBoundKnown: true, ClockSkewMillis: 1000}
	b, _ := json.Marshal(d)
	path := filepath.Join(dir, "deployment.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDeployment(path)
	if err != nil {
		t.Fatal(err)
	}
	relative := d
	relative.CAFile = filepath.Join("secret", "issuer.crt")
	rb, _ := json.Marshal(relative)
	relativePath := filepath.Join(dir, "relative.json")
	if err := os.WriteFile(relativePath, rb, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDeployment(relativePath); err == nil {
		t.Fatal("accepted relative secret path")
	}
	if got.PrivateListen != ":32401" || got.ProbeListen != ":32402" || got.DrainSeconds != 120 {
		t.Fatal("missing deployment defaults")
	}
	// Stale PVC control never provides an acknowledgement for a new process.
	if err := WriteDrainControl(dir, DrainStatusFile, DrainControl{Boot: "boot1", State: "running"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RequestDrain(ctx, dir) }()
	var req DrainControl
	// ACL protection and fsync can take longer than 100 short sleeps on
	// Windows runners. Await the request within the operation deadline.
	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("drain ended before publishing its request: %v", err)
		default:
		}
		req, err = ReadDrainControl(dir, DrainRequestFile)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if req.Boot != "boot1" || req.Operation == "" {
		t.Fatal("unbound drain request")
	}
	if err := WriteDrainControl(dir, DrainStatusFile, DrainControl{Boot: "boot2", Operation: req.Operation, State: "drained"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("new incarnation acknowledged old request")
	}
	unsafe := filepath.Join(dir, DrainStatusFile)
	if err := os.Chmod(unsafe, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDrainControl(dir, DrainStatusFile); err == nil && runtime.GOOS != "windows" {
		t.Fatal("accepted unprotected status")
	}
}

// PostgreSQL authority plus retained local files are the deployment trust
// boundary. This fixture runs in an isolated schema when PG is supplied.
func TestProvisionRestartRecovery(t *testing.T) {
	u := pgSchemaURL(t)
	dir := t.TempDir()
	key := make([]byte, 32)
	key[0] = 1
	o := ProvisionOptions{DataDir: dir, DatabaseURL: u, Key: key, Bootstrap: true, Roles: config.NodeRoles{config.RoleTranscode}, InternalAddress: "https://node.internal:32401"}
	ctx := context.Background()
	if err := Provision(ctx, o); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	machine := cfg.Get().Server.MachineIdentifier
	cfg.Close()
	o.Bootstrap = false
	if err := Provision(ctx, o); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cfg.Close()
	if cfg.Get().Server.MachineIdentifier != machine {
		t.Fatal("restart changed cluster identity")
	}
	other := append([]byte(nil), key...)
	other[0] = 2
	o.Key = other
	if err := Provision(ctx, o); !errors.Is(err, config.ErrKeyMismatch) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	// Simulate a consumed legacy-token crash before any local files existed.
	o.DataDir = t.TempDir()
	o.Key = key
	if err := Provision(ctx, o); err != nil {
		t.Fatal(err)
	}
}

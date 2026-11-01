package playback

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
	"golang.org/x/sys/unix"
)

func TestPayloadFilesystemFreeBSDLocal(t *testing.T) {
	t.Setenv(store.TestPostgresEnv, "")
	dir := t.TempDir()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	name := unix.ByteSliceToString(st.Fstypename[:])
	t.Logf("temporary filesystem: %q, flags=%#x", name, st.Flags)
	if (name != "zfs" && name != "ufs" && name != "tmpfs") || st.Flags&unix.MNT_LOCAL == 0 || st.Flags&unix.MNT_UNION != 0 {
		t.Skip("temporary directory is not on a supported direct local filesystem")
	}
	if ok, err := payloadFilesystemReliable(dir); err != nil || !ok {
		t.Fatalf("local filesystem rejected: ok=%v err=%v", ok, err)
	}
	p, err := openPayloadStore(context.Background(), store.OpenTest(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	if err := p.verifyProof(p.file, p.incarnation); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadFilesystemFreeBSDErrors(t *testing.T) {
	t.Setenv(store.TestPostgresEnv, "")
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(dir, "no-search")
	if err := os.Mkdir(denied, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(denied, 0700); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		name, path string
		want       error
	}{
		{"ENOENT", filepath.Join(dir, "absent"), unix.ENOENT},
		{"ENOTDIR", filepath.Join(file, "child"), unix.ENOTDIR},
		{"EACCES", filepath.Join(denied, "child"), unix.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == unix.EACCES && os.Geteuid() == 0 {
				t.Skip("root bypasses directory search permissions")
			}
			if ok, err := payloadFilesystemReliable(tc.path); ok || err != tc.want {
				t.Errorf("filesystem error: ok=%v err=%v want=%v", ok, err, tc.want)
			}
			if p, err := openPayloadStore(context.Background(), nil, tc.path); p != nil || err != tc.want {
				t.Errorf("store error before DB access: store=%v err=%v want=%v", p, err, tc.want)
			}
			p := &payloadStore{dir: tc.path}
			if err := p.verifyProof(nil, ""); err != tc.want {
				t.Errorf("proof error before witness access: err=%v want=%v", err, tc.want)
			}
			if err := p.reconcileSlots(context.Background()); err != tc.want {
				t.Errorf("recovery error before slot access: err=%v want=%v", err, tc.want)
			}
		})
	}
}

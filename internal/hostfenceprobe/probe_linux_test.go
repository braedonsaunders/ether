//go:build linux

package hostfenceprobe

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBindingRejectsAmbiguousUnboundedAndUnsafeInputs(t *testing.T) {
	b := Binding{Version: 1, HostBoot: "host-boot", DaemonID: "daemon", ContainerID: strings.Repeat("a", 64), ContainerStart: "2026-11-05T16:28:21.218723485Z", PID: 123, Cgroup: "/system.slice/docker-exact.scope"}
	raw, _ := json.Marshal(b)
	if got, err := ReadBinding(strings.NewReader(string(raw))); err != nil || got != b {
		t.Fatalf("valid: %v", err)
	}
	for _, raw := range []string{string(raw) + "{}", strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(raw), `"version":1`, `"unknown":true,"version":1`, 1), strings.Repeat(" ", MaxBinding+1), `null`, `{"version":1`} {
		if _, err := ReadBinding(strings.NewReader(raw)); err == nil {
			t.Fatal("accepted ambiguous or malformed binding")
		}
	}
	for _, cg := range []string{"/", "relative", "/a/../b", "/a//b", "/a/", "/a\x00b", "/a\nb"} {
		bad := b
		bad.Cgroup = cg
		if bad.Validate() == nil {
			t.Fatalf("unsafe cgroup %q", cg)
		}
	}
}

func TestKernelIdentityParsersFailClosed(t *testing.T) {
	// A process name containing a closing parenthesis must not shift starttime.
	stat := "123 (a ) tricky process) S " + strings.Repeat("0 ", 18) + "456 0"
	if ticks, err := statTicks(stat, 123); err != nil || ticks != 456 {
		t.Fatalf("stat: %d %v", ticks, err)
	}
	if _, err := statTicks(stat, 124); err == nil {
		t.Fatal("wrong PID accepted")
	}
	if _, err := statTicks("123 (short) S 1", 123); err == nil {
		t.Fatal("short stat accepted")
	}
	if !namespaceInit("NSpid:\t123\t1\n", 123) || namespaceInit("NSpid:\t123\n", 123) || namespaceInit("NSpid:\t124\t1\n", 123) {
		t.Fatal("namespace init ancestry")
	}
	if !observerVisible("NSpid:\t123\n", 123) || observerVisible("NSpid:\t456\t123\n", 123) {
		t.Fatal("private observer allowed")
	}
	for _, events := range []string{"", "frozen 0", "populated 0\npopulated 1", "populated 2", "populated"} {
		if _, err := populated(events); err == nil {
			t.Fatalf("events accepted: %q", events)
		}
	}
	for _, v := range []string{"0", "1"} {
		if got, err := populated("populated " + v + "\nfrozen 0\n"); err != nil || got != v {
			t.Fatal("valid populated rejected")
		}
	}
}

func TestFilesystemSubstitutionNeverCompletes(t *testing.T) {
	b := Binding{Version: 1, HostBoot: "host-boot", DaemonID: "daemon", ContainerID: strings.Repeat("a", 64), ContainerStart: "start", PID: 123, Cgroup: "/exact"}
	r, err := Probe(t.TempDir(), t.TempDir(), b)
	if err == nil || r.AccessComplete || r.Stage != "host-boot" {
		t.Fatalf("ordinary directory accepted: %+v %v", r, err)
	}
}

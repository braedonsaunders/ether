package plexguid

import "testing"

func TestClientIdentifierPrefersInstallThenMachine(t *testing.T) {
	if got := ClientIdentifier(" install-id ", "machine"); got != "install-id" {
		t.Fatalf("got %q", got)
	}
	if got := ClientIdentifier(" ", "machine-id"); got != "machine-id" {
		t.Fatalf("got %q", got)
	}
	if a, b := ClientIdentifier("", ""), ClientIdentifier("", ""); a == "" || a == b || a == "ether-metadata" {
		t.Fatalf("fallback ids must be unique and non-empty: %q %q", a, b)
	}
	if got := ClientIdentifier("", ""); got == "" {
		t.Fatalf("got %q", got)
	}
}

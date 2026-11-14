package devreference

import (
	"bytes"
	"os"
	"testing"
)

func TestReferenceMatchesContract(t *testing.T) {
	b, err := Generate("../../..")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../../web/src/pages/plugins/developer-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), b) {
		t.Fatal("Plugin API reference is stale: run go generate ./internal/plugins/devreference (covers every capability, host service, event, manifest rule, descriptor and SDK type)")
	}
}

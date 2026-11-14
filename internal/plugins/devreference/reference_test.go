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
	short, full, err := MachineReference("../../..", b)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"llms.txt": short, "llms-full.txt": full} {
		got, err := os.ReadFile("../../../web/public/plugins/reference/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s is stale: run go generate ./internal/plugins/devreference", name)
		}
	}

}

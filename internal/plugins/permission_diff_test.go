package plugins

import (
	"archive/zip"
	"bytes"
	"testing"
)

func TestPermissionDiffCoversCapabilitiesToolsAndLimits(t *testing.T) {
	reviewed := CatalogueArtifact{Kind: KindIntegration, Capabilities: []Capability{CapStreamSource}}
	man := Manifest{
		ID: "a.b",
		Ether: &EtherBlock{
			Kind:         KindIntegration,
			Capabilities: []Capability{CapStreamSource, CapTools, CapFrontendAdapter, CapKindResolver, CapDebridProvider, CapUI},
		},
	}
	if d := catalogueReviewDiff(reviewed, man); len(d) < 5 {
		t.Fatalf("catalogueReviewDiff missed added capabilities: %v", d)
	}

	cur := Manifest{
		ID: "a.b",
		Ether: &EtherBlock{
			Kind:         KindExtension,
			Capabilities: []Capability{CapTools},
			Runtime:      RuntimeDecl{Kind: "hosted", Limits: &RuntimeLimits{MemoryBytes: 32 << 20, Deadline: "10s"}},
			Contributes:  Contributes{Tools: []ToolMeta{{Name: "read_info"}}},
		},
	}
	next := Manifest{
		ID: "a.b",
		Ether: &EtherBlock{
			Kind:         KindExtension,
			Capabilities: []Capability{CapTools},
			Runtime:      RuntimeDecl{Kind: "hosted", Limits: &RuntimeLimits{MemoryBytes: 512 << 20, Deadline: "1h"}},
			Contributes:  Contributes{Tools: []ToolMeta{{Name: "read_info"}, {Name: "delete_all"}}},
		},
	}
	diff := PermissionDiff(cur, next)
	if len(diff) < 3 {
		t.Fatalf("PermissionDiff missed new tool or runtime limit increases: %v", diff)
	}
}

func TestReadPkgRejectsUnsafePath(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w1, _ := zw.Create("manifest.json")
	_, _ = w1.Write([]byte(`{"id":"a.b"}`))
	w2, _ := zw.Create("../escape.txt")
	_, _ = w2.Write([]byte("x"))
	_ = zw.Close()

	if _, _, err := readPkg(buf.Bytes()); err == nil {
		t.Fatal("readPkg silently accepted zip with unsafe path")
	}
}

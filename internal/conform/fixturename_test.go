package conform

import (
	"strings"
	"testing"
)

func TestFixtureNameFitsWindows(t *testing.T) {
	long := "/video/:/transcode/universal/decision?hasMDE=1&path=%2Flibrary%2Fmetadata%2F{{movieKey}}&mediaIndex=0&partIndex=0&protocol=dash&fastSeek=1&directPlay=1&directStream=1"
	n := FixtureName("appletv", long, true)
	if len(n) > maxFixtureName || !strings.HasPrefix(n, "appletv_json_video__transcode_universal_decision") {
		t.Fatalf("name %q (%d chars)", n, len(n))
	}
	if n != FixtureName("appletv", long, true) {
		t.Fatal("not deterministic")
	}
	if FixtureName("appletv", long+"&x=1", true) == n {
		t.Fatal("distinct long requests collided")
	}
	if got := FixtureName("web", "/library/sections", false); got != "web_xml_library_sections" {
		t.Fatalf("short names unchanged, got %q", got)
	}
	if strings.ContainsAny(FixtureName("web", `/a*b|c<d>e"f\g`, false), `*|<>"\`) {
		t.Fatal("Windows-forbidden characters kept")
	}
}

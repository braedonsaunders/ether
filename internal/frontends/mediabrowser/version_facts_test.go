package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"testing"
)

func TestVersionFactsExposeOnlyKnownBasenamesAndSingleVersionPartCount(t *testing.T) {
	count := 2
	i := &instance{protocol: "jellyfin"}
	s := map[string]any{}
	v := frontend.Version{FileName: "Film.mkv", ETag: "revision", PartCount: &count}
	i.versionSourceFacts(s, v)
	if s["Name"] != "Film.mkv" || s["ETag"] != "revision" {
		t.Fatal(s)
	}
	dto := map[string]any{}
	i.versionItemFacts(dto, []frontend.Version{v})
	if dto["PartCount"] != 2 {
		t.Fatal(dto)
	}
	dto = map[string]any{}
	i.versionItemFacts(dto, []frontend.Version{v, v})
	if _, ok := dto["PartCount"]; ok {
		t.Fatal(dto)
	}
	s = map[string]any{}
	i.versionSourceFacts(s, frontend.Version{FileName: "/secret/file.mkv"})
	if _, ok := s["Name"]; ok {
		t.Fatal(s)
	}
	s = map[string]any{}
	(&instance{protocol: "emby"}).versionSourceFacts(s, v)
	if _, ok := s["ETag"]; ok {
		t.Fatal(s)
	}
}

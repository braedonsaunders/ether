package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordedProfileBuildsAlignedReadOnlyRequests(t *testing.T) {
	file := filepath.Join("..", "..", "internal", "conform", "profiles", "jellyfin", "swiftfin.json")
	s := scenario{Name: "resume", ProfileFile: file, Flow: "home", Probe: "resume", Reference: target{URL: "http://reference", Variables: map[string]string{"user_id": "ref", "device_id": "device"}}, Ether: target{URL: "http://ether", Variables: map[string]string{"user_id": "target", "device_id": "device"}}}
	got, e := profileScenario(s)
	if e != nil {
		t.Fatal(e)
	}
	if got.Reference.URL != "http://reference/Users/ref/Items/Resume" || got.Ether.URL != "http://ether/Users/target/Items/Resume" || !strings.Contains(got.Ether.Headers["Authorization"], "Swiftfin") {
		t.Fatalf("wrong profile mapping: %+v", got)
	}
	s.Flow = "sign_in"
	s.Probe = "authenticate"
	if _, e = profileScenario(s); e == nil {
		t.Fatal("mutating request accepted")
	}
}
func TestProfilePathEscapesPerContext(t *testing.T) {
	got, e := expandProfilePath("/Items/${x}?search=${x}", map[string]string{"x": "a b"})
	if e != nil || got != "/Items/a%20b?search=a+b" {
		t.Fatalf("%q %v", got, e)
	}
	if _, e = expandProfilePath("/Items/${missing}", nil); e == nil {
		t.Fatal("missing variable accepted")
	}
}

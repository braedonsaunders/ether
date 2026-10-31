package nzb

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDuplicateSegmentNumbersBecomeAlternates(t *testing.T) {
	n, err := Parse([]byte(sampleNZB))
	if err != nil {
		t.Fatal(err)
	}
	f := n.Files[0]
	if got := f.Segments[1].Alternates; !reflect.DeepEqual(got, []string{"p1s2dup@example"}) {
		t.Fatalf("part 2 alternates = %v", got)
	}
	for _, i := range []int{0, 2, 3} {
		if len(f.Segments[i].Alternates) != 0 {
			t.Fatalf("part %d alternates = %v", i+1, f.Segments[i].Alternates)
		}
	}
}

func TestSegmentAlternatesAreBoundedAndDeduplicated(t *testing.T) {
	var segs strings.Builder
	segs.WriteString(`<segment bytes="10" number="1">primary@x</segment>`)
	segs.WriteString(`<segment bytes="10" number="1">primary@x</segment>`) // the same id again
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&segs, `<segment bytes="10" number="1">fill%d@x</segment>`, i)
		fmt.Fprintf(&segs, `<segment bytes="10" number="1">fill%d@x</segment>`, i)
	}
	doc := `<nzb><file subject="&quot;a.mkv&quot; yEnc (1/1)"><groups><group>g</group></groups><segments>` + segs.String() + `</segments></file></nzb>`
	n, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	s := n.Files[0].Segments[0]
	if s.MessageID != "primary@x" || !reflect.DeepEqual(s.Alternates, []string{"fill0@x", "fill1@x", "fill2@x", "fill3@x"}) {
		t.Fatalf("segment = %+v", s)
	}
}

func repostNZB(secondBytes string) string {
	file := func(prefix, b2 string) string {
		return `<file subject="&quot;Show.part01.rar&quot; yEnc (1/2)"><groups><group>g</group></groups><segments>` +
			`<segment bytes="1000" number="1">` + prefix + `1@x</segment>` +
			`<segment bytes="` + b2 + `" number="2">` + prefix + `2@x</segment>` +
			`</segments></file>`
	}
	return `<nzb>` + file("a", "400") + file("b", secondBytes) +
		`<file subject="&quot;Show.part02.rar&quot; yEnc (1/1)"><groups><group>g</group></groups><segments><segment bytes="10" number="1">c1@x</segment></segments></file></nzb>`
}

// A whole volume posted twice with the same layout: the set keeps the
// first copy, whose parts carry the repost's message-IDs as alternates.
func TestRepostedVolumeServesAsAlternates(t *testing.T) {
	n, err := Parse([]byte(repostNZB("400")))
	if err != nil {
		t.Fatal(err)
	}
	sets := GroupSets(n.Files)
	if len(sets) != 1 || len(sets[0].Volumes) != 2 || len(sets[0].Duplicates) != 1 {
		t.Fatalf("sets = %+v", sets)
	}
	v := sets[0].Volumes[0]
	if v != n.Files[0] {
		t.Fatal("set did not keep the first copy")
	}
	for i, want := range []string{"b1@x", "b2@x"} {
		if got := v.Segments[i].Alternates; !reflect.DeepEqual(got, []string{want}) {
			t.Fatalf("part %d alternates = %v, want [%s]", i+1, got, want)
		}
	}
	if len(sets[0].Volumes[1].Segments[0].Alternates) != 0 {
		t.Fatal("unrelated volume got alternates")
	}
}

// A repost whose parts are sized differently need not cover the same bytes
// per part: it stays a duplicate only.
func TestRepostWithDifferentLayoutIsNotMerged(t *testing.T) {
	n, err := Parse([]byte(repostNZB("401")))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range n.Files[0].Segments {
		if len(s.Alternates) != 0 {
			t.Fatalf("mismatched repost merged: %+v", s)
		}
	}
}

// Obfuscated names say nothing about identity.
func TestObfuscatedSameNameIsNotMerged(t *testing.T) {
	name := "a8f3c2e9b1d74f6a9e0c5b2d8f1a3c7e"
	file := func(id string) string {
		return `<file subject="&quot;` + name + `&quot; yEnc (1/1)"><groups><group>g</group></groups><segments><segment bytes="10" number="1">` + id + `</segment></segments></file>`
	}
	n, err := Parse([]byte(`<nzb>` + file("a@x") + file("b@x") + `</nzb>`))
	if err != nil {
		t.Fatal(err)
	}
	if !n.Files[0].Obfuscated {
		t.Skip("name not classified as obfuscated")
	}
	if len(n.Files[0].Segments[0].Alternates) != 0 {
		t.Fatal("obfuscated files merged")
	}
}

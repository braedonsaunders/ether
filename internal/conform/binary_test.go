package conform

import (
	"net/http"
	"testing"
)

func TestBinaryRangeChecksSemanticsWithoutComparingHashes(t *testing.T) {
	a := Response{Kind: "binary-range", RangeKind: "start", Status: 206, BodyLength: 3, SHA256: "a", Headers: http.Header{"Content-Range": []string{"bytes 0-2/3"}, "Content-Length": []string{"3"}, "Content-Type": []string{"video/mp4"}}}
	b := a
	b.Headers = a.Headers.Clone()
	b.SHA256 = "different"
	if r := CompareBinary(a, b); r.Blocking() != 0 {
		t.Fatalf("hash differences should not block: %+v", r)
	}
	b.Headers.Set("Content-Range", "bytes 0-9/10")
	if r := CompareBinary(a, b); r.Blocking() == 0 {
		t.Fatal("invalid body range accepted")
	}
}
func TestReferenceDefectDoesNotRelaxTarget(t *testing.T) {
	defect := &ReferenceDefect{ID: "emby-suffix-range-ignored", Detail: "suffix ignored", Observed: "4.10.2"}
	reference := Response{Kind: "binary-range", RangeKind: "suffix", Status: 206, BodyLength: 65537, Truncated: true, ReferenceDefect: defect, Headers: http.Header{"Content-Range": []string{"bytes 0-65536/200000"}, "Content-Length": []string{"65537"}}}
	target := Response{Kind: "binary-range", RangeKind: "suffix", Status: 206, BodyLength: 65536, Headers: http.Header{"Content-Range": []string{"bytes 134464-199999/200000"}, "Content-Length": []string{"65536"}}}
	r := CompareBinary(reference, target)
	if r.Blocking() != 0 || len(r.ReferenceDefects) != 1 {
		t.Fatalf("valid target: %+v", r)
	}
	bad := reference
	r = CompareBinary(reference, bad)
	if r.Blocking() == 0 {
		t.Fatal("reference defect was allowed on target")
	}
}

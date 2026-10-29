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

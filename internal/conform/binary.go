package conform

import (
	"fmt"
	"strconv"
	"strings"
)

// CompareBinary compares range semantics, not media bytes or cross-server hashes.
func CompareBinary(a, b Response) Report {
	var r Report
	add := func(path, kind string, ref, target any) {
		r.Differences = append(r.Differences, Difference{Path: path, Kind: kind, Reference: ref, Target: target, Blocking: true})
	}
	if a.Kind != "binary-range" || b.Kind != "binary-range" {
		add("$kind", KindType, a.Kind, b.Kind)
		return r
	}
	if a.RangeKind != b.RangeKind {
		add("$rangeKind", KindValue, a.RangeKind, b.RangeKind)
	}
	if a.Status != b.Status {
		add("$status", KindStatus, a.Status, b.Status)
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Cache-Control"} {
		x, y := a.Headers.Get(name), b.Headers.Get(name)
		if (x == "") != (y == "") {
			add("$headers."+name, KindHeader, x != "", y != "")
		} else if name == "Content-Type" && x != y {
			add("$headers."+name, KindHeader, x, y)
		}
	}
	if err := checkRange(a); err != nil {
		add("$reference.range", KindHeader, "valid", err.Error())
	}
	if err := checkRange(b); err != nil {
		add("$target.range", KindHeader, "valid", err.Error())
	}
	return r
}
func checkRange(r Response) error {
	if r.Status != 206 {
		return fmt.Errorf("expected status 206, got %d", r.Status)
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(r.Headers.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
		return fmt.Errorf("invalid Content-Range")
	}
	if total <= 0 || start < 0 || end < start || end >= total || end-start+1 != r.BodyLength || r.BodyLength > 65536 {
		return fmt.Errorf("inconsistent Content-Range and body length")
	}
	if v := r.Headers.Get("Content-Length"); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n != r.BodyLength {
			return fmt.Errorf("inconsistent Content-Length")
		}
	}
	switch r.RangeKind {
	case "start":
		if start != 0 {
			return fmt.Errorf("range does not start at zero")
		}
	case "middle":
		if start != total/2 {
			return fmt.Errorf("range does not start at midpoint")
		}
	case "suffix":
		if end != total-1 {
			return fmt.Errorf("range does not end at file end")
		}
	default:
		return fmt.Errorf("unknown range kind")
	}
	return nil
}

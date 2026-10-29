package conform

import (
	"fmt"
	"strconv"
	"strings"
)

// ReferenceDefect records a known behavior of a reference, never an Ether requirement.
type ReferenceDefect struct {
	ID       string `json:"id"`
	Detail   string `json:"detail"`
	Observed string `json:"observed"`
}

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
	if a.ReferenceDefect != nil && knownDefect(a) {
		r.ReferenceDefects = append(r.ReferenceDefects, *a.ReferenceDefect)
	} else if err := checkRange(a); err != nil {
		add("$reference.range", KindHeader, "valid", err.Error())
	}
	if err := checkRange(b); err != nil {
		add("$target.range", KindHeader, "valid", err.Error())
	}
	return r
}
func knownDefect(r Response) bool {
	if r.ReferenceDefect == nil || r.ReferenceDefect.ID != "emby-suffix-range-ignored" || r.RangeKind != "suffix" || r.Status != 206 || !r.Truncated || r.BodyLength != 65537 || r.Headers.Get("Content-Length") != "65537" {
		return false
	}
	var first, last, total int64
	_, err := fmt.Sscanf(r.Headers.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &total)
	return err == nil && first == 0 && last == 65536 && total > 65537
}

// ValidateBinary checks a live response against strict byte-range semantics.
func ValidateBinary(r Response) error { return checkRange(r) }

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
	case "captured":
		if err := checkRequestedRange(r.RequestRange, start, end, total); err != nil {
			return err
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

func checkRequestedRange(value string, start, end, total int64) error {
	if !strings.HasPrefix(value, "bytes=") {
		return fmt.Errorf("missing captured Range")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return fmt.Errorf("invalid captured Range")
	}
	last := total - 1
	if parts[1] != "" {
		var err error
		last, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || last < 0 {
			return fmt.Errorf("invalid captured Range")
		}
	} else if parts[0] == "" {
		return fmt.Errorf("invalid captured Range")
	}
	if parts[0] == "" {
		expected := total - last
		if expected < 0 {
			expected = 0
		}
		if start != expected || end != total-1 {
			return fmt.Errorf("suffix response mismatch")
		}
		return nil
	}
	first, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid captured Range")
	}
	if last >= total {
		last = total - 1
	}
	if start != first || end != last {
		return fmt.Errorf("range response mismatch")
	}
	return nil
}

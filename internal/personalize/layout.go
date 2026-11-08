package personalize

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// ValidationError lists problems with an owner-supplied document (Home
// layout, taste overrides), addressed by dotted field path.
type ValidationError struct {
	Problems []Problem
}

// Problem is one validation failure.
type Problem struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 0 {
		return "invalid"
	}
	p := e.Problems[0]
	return p.Field + ": " + p.Message
}

// InvalidFields returns (field, message) pairs; the admin API maps them to
// its validation error shape.
func (e *ValidationError) InvalidFields() [][2]string {
	out := make([][2]string, len(e.Problems))
	for i, p := range e.Problems {
		out[i] = [2]string{p.Field, p.Message}
	}
	return out
}

func (e *ValidationError) add(field, format string, args ...any) {
	e.Problems = append(e.Problems, Problem{field, fmt.Sprintf(format, args...)})
}

func (e *ValidationError) orNil() error {
	if len(e.Problems) == 0 {
		return nil
	}
	return e
}

var validDevices = map[string]bool{core.DeviceTV: true, core.DeviceMobile: true, core.DeviceWeb: true, core.DeviceDesktop: true}

// validateWhen checks a row schedule; field is the dotted path of the
// schedule in the owner's document.
func validateWhen(ve *ValidationError, field string, w *core.LayoutWhen) {
	for _, d := range w.Days {
		if d < 0 || d > 6 {
			ve.add(field+".days", "days are 0 (Sunday) to 6 (Saturday)")
			break
		}
	}
	if w.FromHour != nil && (*w.FromHour < 0 || *w.FromHour > 23) {
		ve.add(field+".fromHour", "must be 0-23")
	}
	if w.ToHour != nil && (*w.ToHour < 0 || *w.ToHour > 24) {
		ve.add(field+".toHour", "must be 0-24")
	}
	for _, d := range w.Devices {
		if !validDevices[d] {
			ve.add(field+".devices", "devices are tv, mobile, web or desktop")
			break
		}
	}
}

type decision struct {
	hidden, pinned bool
	position       int
}

// matchRank reports how specifically a rule's hubId targets a row (the
// console's matchRank in web/src/lib/layout.ts ranks the same way): 3 the
// full hub id, 2 the kind, 1 the hub id without its screen prefix or a glob
// over any of them, 0 no match. stem is hubStem(hubID), so "because.*"
// covers "home.because.123" and "movie.because.123".
func matchRank(pattern, hubID, stem, kind string) int {
	switch pattern {
	case hubID:
		return 3
	case kind:
		return 2
	case stem:
		return 1
	}
	if !isGlob(pattern) {
		return 0
	}
	for _, cand := range [...]string{hubID, stem, kind} {
		if ok, _ := path.Match(pattern, cand); ok {
			return 1
		}
	}
	return 0
}

// isGlob reports whether a rule uses path.Match metacharacters. A byte loop
// rather than strings.ContainsAny, which builds a lookup set on every call.
func isGlob(pattern string) bool {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*', '?', '[':
			return true
		}
	}
	return false
}

// hubStem is a hub id without its screen prefix ("home.because.123" →
// "because.123").
func hubStem(hubID string) string {
	for _, p := range []string{"home.", "movie.", "tv."} {
		if s, ok := strings.CutPrefix(hubID, p); ok {
			return s
		}
	}
	return hubID
}

// whenMatches evaluates a schedule against the request context.
func whenMatches(w *core.LayoutWhen, hc core.HubContext) bool {
	if w == nil {
		return true
	}
	if len(w.Months) > 0 {
		ok := false
		for _, m := range w.Months {
			if m == int(hc.Now.Month()) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(w.Dayparts) > 0 {
		ok := false
		for _, d := range w.Dayparts {
			if d == core.DaypartAt(hc.Now) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	date := hc.Now.Format("2006-01-02")
	if w.StartDate != "" && date < w.StartDate || w.EndDate != "" && date > w.EndDate {
		return false
	}
	if len(w.Days) > 0 {
		ok := false
		for _, d := range w.Days {
			if d == int(hc.Now.Weekday()) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if w.FromHour != nil || w.ToHour != nil {
		from, to := 0, 24
		if w.FromHour != nil {
			from = *w.FromHour
		}
		if w.ToHour != nil {
			to = *w.ToHour
		}
		h := hc.Now.Hour()
		if from <= to {
			if h < from || h >= to {
				return false
			}
		} else if h < from && h >= to { // wraps midnight
			return false
		}
	}
	if len(w.Devices) > 0 {
		ok := false
		for _, d := range w.Devices {
			if d == hc.Device {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// placePinned inserts pinned rows at their positions (0 = top) into the
// ranked list, in ascending position order.
func placePinned[T any](ranked []T, pinned []T, pos func(T) int) []T {
	sort.SliceStable(pinned, func(i, j int) bool { return pos(pinned[i]) < pos(pinned[j]) })
	out := append([]T{}, ranked...)
	for _, p := range pinned {
		i := pos(p)
		if i > len(out) {
			i = len(out)
		}
		out = append(out, p)
		copy(out[i+1:], out[i:])
		out[i] = p
	}
	return out
}

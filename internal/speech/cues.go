package speech

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type cue struct {
	start, end time.Duration
	text       string
}

func parseCues(text string) ([]cue, error) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	blocks := strings.Split(text, "\n\n")
	var out []cue
	for _, blk := range blocks {
		lines := strings.Split(strings.Trim(blk, "\n"), "\n")
		for i, ln := range lines {
			start, end, ok := cueTimes(ln)
			if !ok || i+1 >= len(lines) {
				continue
			}
			body := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
			if body == "" || end <= start {
				continue
			}
			out = append(out, cue{start: start, end: end, text: body})
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("speech-to-text returned no timed cues")
	}
	return out, nil
}

func cueTimes(ln string) (start, end time.Duration, ok bool) {
	parts := strings.Split(ln, "-->")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, ok1 := parseStamp(strings.TrimSpace(parts[0]))
	end, ok2 := parseStamp(strings.TrimSpace(parts[1]))
	return start, end, ok1 && ok2
}

func parseStamp(s string) (time.Duration, bool) {
	s = strings.ReplaceAll(s, ",", ".")
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	h, m, sec := 0, 0, 0
	bits := strings.Split(s, ":")
	switch len(bits) {
	case 3:
		h, _ = strconv.Atoi(bits[0])
		m, _ = strconv.Atoi(bits[1])
		s = bits[2]
	case 2:
		m, _ = strconv.Atoi(bits[0])
		s = bits[1]
	default:
		return 0, false
	}
	frac := "0"
	if i := strings.IndexByte(s, '.'); i >= 0 {
		frac = s[i+1:]
		s = s[:i]
	}
	sec, _ = strconv.Atoi(s)
	for len(frac) < 3 {
		frac += "0"
	}
	ms, _ := strconv.Atoi(frac[:3])
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond, true
}

func renderVTT(cues []cue) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", stamp(c.start), stamp(c.end), c.text)
	}
	return b.String()
}

func stamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	ms := (d % time.Second) / time.Millisecond
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

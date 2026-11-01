package subtitles

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MovieHash reads exactly the first and last 64 KiB. Call only on cached
// source ranges so hashing does not fetch a release during first-frame startup.
// Implemented from the published OpenSubtitles algorithm, without copied code.
func MovieHash(r io.ReaderAt, size int64) (string, error) {
	if size < 131072 {
		return "", errors.New("moviehash requires at least 128 KiB")
	}
	sum := uint64(size)
	buf := make([]byte, 65536)
	for _, off := range []int64{0, size - 65536} {
		n, err := r.ReadAt(buf, off)
		if n != len(buf) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return "", err
		}
		for i := 0; i < len(buf); i += 8 {
			sum += binary.LittleEndian.Uint64(buf[i : i+8])
		}
	}
	return fmt.Sprintf("%016x", sum), nil
}

// ParseASS converts Dialogue events using the declared Format field. Styling,
// positioning and karaoke effects are deliberately dropped for text fallbacks.
func ParseASS(text string) ([]Cue, error) {
	var out []Cue
	var fields []string
	events := false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			events = strings.EqualFold(line, "[Events]")
			continue
		}
		if !events {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.EqualFold(name, "Format") {
			fields = strings.Split(strings.ToLower(value), ",")
			for i := range fields {
				fields[i] = strings.TrimSpace(fields[i])
			}
			continue
		}
		if !strings.EqualFold(name, "Dialogue") || len(fields) == 0 {
			continue
		}
		parts := strings.SplitN(value, ",", len(fields))
		if len(parts) != len(fields) {
			continue
		}
		var start, end time.Duration
		body := ""
		for i, f := range fields {
			switch f {
			case "start":
				start = assTime(parts[i])
			case "end":
				end = assTime(parts[i])
			case "text":
				body = parts[i]
			}
		}
		body = tagRe.ReplaceAllString(body, "")
		body = strings.NewReplacer(`\N`, "\n", `\n`, "\n", `\h`, " ").Replace(body)
		if start >= 0 && end > start && strings.TrimSpace(body) != "" {
			out = append(out, Cue{start, end, body})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no ASS dialogue cues found")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}
func assTime(raw string) time.Duration {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 3 {
		return -1
	}
	h, e1 := strconv.ParseInt(parts[0], 10, 32)
	m, e2 := strconv.ParseInt(parts[1], 10, 32)
	sec, e3 := strconv.ParseFloat(parts[2], 64)
	if e1 != nil || e2 != nil || e3 != nil || h < 0 || h > 999 || m < 0 || m > 59 || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 || sec >= 60 {
		return -1
	}
	// Round to whole milliseconds: 2.01 s is 2.009999999 s in float64, which
	// would truncate to a cue one millisecond early.
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(math.Round(sec*1000))*time.Millisecond
}

// Shift applies an offset and playback-rate correction without mutating input.
// Negative cues are dropped or clipped, preserving valid SRT/WebVTT times.
func Shift(cues []Cue, offset time.Duration, rate float64) ([]Cue, error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0.5 || rate > 2 {
		return nil, errors.New("subtitle rate must be between 0.5 and 2")
	}
	var out []Cue
	for _, c := range cues {
		c.Start = time.Duration(float64(c.Start)*rate) + offset
		c.End = time.Duration(float64(c.End)*rate) + offset
		if c.End <= 0 {
			continue
		}
		if c.Start < 0 {
			c.Start = 0
		}
		if c.End > c.Start {
			out = append(out, c)
		}
	}
	return out, nil
}

// SyncHandler serves GET / of cached sidecars and POST /{id} as a
// manual timing adjust written into the correction store.
func (s *Service) SyncHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Trim(r.URL.Path, "/") == "" {
			items, err := s.CachedSubtitles()
			if err != nil {
				http.Error(w, "Subtitles unavailable", 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(items)
			return
		}
		if r.Method == http.MethodPost {
			s.handleManualAdjust(w, r)
			return
		}
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
}

func (s *Service) handleManualAdjust(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(strings.Trim(r.URL.Path, "/"), URLPrefix)
	if id == "" || strings.ContainsAny(id, "/\\") {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if strings.Contains(id, "..") {
		http.Error(w, "Invalid subtitle id", http.StatusBadRequest)
		return
	}
	var body struct {
		OffsetMs  int64   `json:"offsetMs"`
		Rate      float64 `json:"rate"`
		Reference string  `json:"reference"`
		Apply     bool    `json:"apply"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if body.OffsetMs < -3600000 || body.OffsetMs > 3600000 {
		http.Error(w, "Offset must be within ±1 hour", http.StatusBadRequest)
		return
	}
	rate := body.Rate
	if rate == 0 {
		rate = 1
	}
	if rate < 0.5 || rate > 2 {
		http.Error(w, "Rate must be between 0.5 and 2", http.StatusBadRequest)
		return
	}
	c := correction{
		Offset: time.Duration(body.OffsetMs) * time.Millisecond,
		Rate:   rate, Applied: body.Apply, Manual: true, Measured: false,
		Confidence: 1, Inliers: 1, At: s.now().UTC(),
	}
	if body.Reference != "" {
		cues, err := s.cuesByCachedID(id)
		ref, rerr := parseReference(body.Reference)
		if rerr != nil {
			http.Error(w, rerr.Error(), http.StatusBadRequest)
			return
		}
		if err == nil {
			if fit, ok := FitTiming(cues, ref); ok {
				c.Offset, c.Rate = fit.Offset, fit.Rate
				c.Confidence, c.Inliers = fit.Confidence, fit.Inliers
			}
		}
	}
	s.storeCorrection(id, manualSource, c)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"externalUrl": URLPrefix + id})
}

func parseReference(text string) ([]Cue, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("Reference subtitles are empty")
	}
	if looksASS(text) {
		cues, err := ParseASS(text)
		if err != nil || len(cues) == 0 {
			return nil, errors.New("Reference subtitles must be readable ASS, SRT, or WebVTT")
		}
		return cues, nil
	}
	cues, err := ParseCues(text)
	if err != nil || len(cues) == 0 {
		return nil, errors.New("Reference subtitles must be readable ASS, SRT, or WebVTT")
	}
	return cues, nil
}

func looksASS(s string) bool {
	return strings.Contains(s, "[Script Info]") || strings.Contains(s, "[Events]") || strings.Contains(s, "Dialogue:")
}

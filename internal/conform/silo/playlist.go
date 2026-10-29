package silo

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
)

type playlistFacts struct {
	segments                  int
	total, target, maxSegment float64
	complete, master          bool
	variants                  int
	variantAttributes         []string
}

// playlistFacts validates semantic invariants without requiring a server to
// use the same segment boundaries or authorization-templating syntax.
func parsePlaylist(body []byte) (playlistFacts, error) {
	var result playlistFacts
	lines := strings.Split(string(bytes.TrimSpace(body)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return result, fmt.Errorf("missing EXTM3U")
	}
	waitingSegment, waitingVariant := false, false
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			v, e := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")), 64)
			if e != nil || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return result, fmt.Errorf("invalid target duration")
			}
			result.target = v
		case strings.HasPrefix(line, "#EXTINF:"):
			if waitingSegment || waitingVariant {
				return result, fmt.Errorf("missing playlist URI")
			}
			v, e := strconv.ParseFloat(strings.SplitN(strings.TrimPrefix(line, "#EXTINF:"), ",", 2)[0], 64)
			if e != nil || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return result, fmt.Errorf("invalid segment duration")
			}
			if v > result.maxSegment {
				result.maxSegment = v
			}
			result.total += v
			waitingSegment = true
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			if waitingSegment || waitingVariant {
				return result, fmt.Errorf("missing playlist URI")
			}
			bandwidth := false
			attributes := []string{}
			for _, field := range splitAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:")) {
				field = strings.TrimSpace(field)
				if strings.HasPrefix(field, "BANDWIDTH=") {
					n, e := strconv.ParseInt(strings.TrimPrefix(field, "BANDWIDTH="), 10, 64)
					bandwidth = e == nil && n > 0
				}
				if strings.HasPrefix(field, "BANDWIDTH=") || strings.HasPrefix(field, "RESOLUTION=") || strings.HasPrefix(field, "CODECS=") {
					attributes = append(attributes, field)
				}
			}
			if !bandwidth {
				return result, fmt.Errorf("variant lacks valid bandwidth")
			}
			result.variantAttributes = append(result.variantAttributes, strings.Join(attributes, ","))
			result.master = true
			waitingVariant = true
		case line == "#EXT-X-ENDLIST":
			result.complete = true
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		default:
			if waitingSegment {
				result.segments++
				waitingSegment = false
			} else if waitingVariant {
				result.variants++
				waitingVariant = false
			} else {
				return result, fmt.Errorf("playlist URI has no segment or variant")
			}
		}
	}
	if waitingSegment || waitingVariant {
		return result, fmt.Errorf("missing playlist URI")
	}
	if result.master {
		if result.variants == 0 || result.segments != 0 {
			return result, fmt.Errorf("invalid master playlist")
		}
	} else if result.segments == 0 || result.target <= 0 {
		return result, fmt.Errorf("incomplete media playlist")
	} else if math.Round(result.maxSegment) > result.target {
		return result, fmt.Errorf("segment exceeds target duration")
	}
	return result, nil
}

func splitAttributes(s string) []string {
	var fields []string
	start := 0
	quoted := false
	for i, r := range s {
		if r == '"' {
			quoted = !quoted
		}
		if r == ',' && !quoted {
			fields = append(fields, s[start:i])
			start = i + 1
		}
	}
	return append(fields, s[start:])
}

func compareSiloPlaylist(a, b conform.Response) conform.Report {
	report := conform.ComparePlaylist(a, b)
	for index := range report.Differences {
		d := &report.Differences[index]
		// These tags describe valid alternative URL templates or encoder
		// boundaries; they do not require the other server to emit the tag.
		if d.Path == "$playlist/#EXT-X-DEFINE" || d.Path == "$playlist/#EXT-X-DISCONTINUITY" || d.Path == "$playlist/#EXT-X-INDEPENDENT-SEGMENTS" {
			d.Blocking = false
		}
	}
	x, xe := parsePlaylist(a.Body)
	y, ye := parsePlaylist(b.Body)
	if xe != nil || ye != nil {
		var reference, target any
		if xe != nil {
			reference = xe.Error()
		}
		if ye != nil {
			target = ye.Error()
		}
		report.Differences = append(report.Differences, conform.Difference{Path: "$playlist/semantics", Kind: conform.KindParse, Reference: reference, Target: target, Blocking: true})
		return report
	}
	if x.master != y.master {
		report.Differences = append(report.Differences, conform.Difference{Path: "$playlist/kind", Kind: conform.KindType, Reference: x.master, Target: y.master, Blocking: true})
		return report
	}
	if !x.master && x.complete && y.complete {
		tolerance := math.Max(1, x.total*0.01)
		if math.Abs(x.total-y.total) > tolerance {
			report.Differences = append(report.Differences, conform.Difference{Path: "$playlist/duration", Kind: conform.KindValue, Reference: x.total, Target: y.total, Blocking: true})
		}
	}
	if x.segments != y.segments {
		report.Differences = append(report.Differences, conform.Difference{Path: "$playlist/segment_count", Kind: conform.KindValue, Reference: x.segments, Target: y.segments, Blocking: false})
	}
	if x.target != y.target {
		report.Differences = append(report.Differences, conform.Difference{Path: "$playlist/target_duration", Kind: conform.KindValue, Reference: x.target, Target: y.target, Blocking: false})
	}
	if x.master && strings.Join(x.variantAttributes, "|") != strings.Join(y.variantAttributes, "|") {
		report.Differences = append(report.Differences, conform.Difference{Path: "$playlist/variant_attributes", Kind: conform.KindValue, Reference: x.variantAttributes, Target: y.variantAttributes, Blocking: false})
	}
	return report
}

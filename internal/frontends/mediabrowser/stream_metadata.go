package mediabrowser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// streamMetadataDTO formats source facts only. Callers suppress estimated
// streams before conversion; this helper neither probes nor changes a plan.
func streamMetadataDTO(dto map[string]any, s frontend.StreamInfo) {
	if ratio := streamAspectRatio(s); ratio != "" {
		dto["AspectRatio"] = ratio
	}
	// Dimensions alone cannot establish pixel shape, progressive scan, SDR or
	// a marketed resolution class. Use literal dimensions and channel counts.
	var parts []string
	if s.Title != "" {
		parts = append(parts, s.Title)
	}
	if s.Codec != "" {
		parts = append(parts, strings.ToUpper(s.Codec))
	}
	switch s.Type {
	case "video":
		if s.Width > 0 && s.Height > 0 {
			parts = append(parts, fmt.Sprintf("%dx%d", s.Width, s.Height))
		}
	case "audio":
		if s.Channels > 0 {
			parts = append(parts, fmt.Sprintf("%d ch", s.Channels))
		}
		if s.SampleRate > 0 {
			parts = append(parts, fmt.Sprintf("%d Hz", s.SampleRate))
		}
	}
	if len(parts) > 0 {
		dto["DisplayTitle"] = strings.Join(parts, " - ")
	}
}

func streamAspectRatio(s frontend.StreamInfo) string {
	if s.Type != "video" {
		return ""
	}
	if s.DisplayAspectRatio != "" {
		return s.DisplayAspectRatio
	}
	// Never assume square pixels when SAR is absent. Bound multiplication and
	// use int64 so dimensions and sample aspect cannot overflow on 32-bit hosts.
	if s.Width <= 0 || s.Height <= 0 || s.Width > 100000 || s.Height > 100000 {
		return ""
	}
	parts := strings.Split(s.SampleAspectRatio, ":")
	if len(parts) != 2 {
		return ""
	}
	n, en := strconv.Atoi(parts[0])
	d, ed := strconv.Atoi(parts[1])
	if en != nil || ed != nil || n <= 0 || d <= 0 || n > 100000 || d > 100000 {
		return ""
	}
	w, h := int64(s.Width)*int64(n), int64(s.Height)*int64(d)
	a, b := w, h
	for b != 0 {
		a, b = b, a%b
	}
	return fmt.Sprintf("%d:%d", w/a, h/a)
}

package diagnostics

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// boundPlayback clips and copies a playback decision so the ring owns it.
func boundPlayback(p *PlaybackDecision, uses []secretUse) *PlaybackDecision {
	if p == nil {
		return nil
	}
	out := *p
	out.Profile = clipFree(out.Profile, 80, uses)
	out.Chooser = clipFree(out.Chooser, 16, uses)
	out.ChosenRelease = clipFree(out.ChosenRelease, maxString, uses)
	if len(out.Candidates) > maxPlaybackCandidates {
		out.Candidates = append([]PlaybackCandidate(nil), out.Candidates[:maxPlaybackCandidates]...)
	} else if len(out.Candidates) > 0 {
		out.Candidates = append([]PlaybackCandidate(nil), out.Candidates...)
	}
	for i := range out.Candidates {
		c := &out.Candidates[i]
		c.Name = clipFree(c.Name, maxString, uses)
		c.Reason = clipFree(c.Reason, maxString, uses)
	}
	if len(out.Variants) > maxPlaybackCandidates {
		out.Variants = append([]PlaybackVariant(nil), out.Variants[:maxPlaybackCandidates]...)
	} else if len(out.Variants) > 0 {
		out.Variants = append([]PlaybackVariant(nil), out.Variants...)
	}
	for i := range out.Variants {
		v := &out.Variants[i]
		v.Release = clipFree(v.Release, maxString, uses)
	}
	return &out
}

func clonePlayback(p *PlaybackDecision) *PlaybackDecision {
	if p == nil {
		return nil
	}
	out := *p
	if len(out.Candidates) > 0 {
		out.Candidates = append([]PlaybackCandidate(nil), out.Candidates...)
	}
	if len(out.Variants) > 0 {
		out.Variants = append([]PlaybackVariant(nil), out.Variants...)
	}
	return &out
}

// exportPlayback redacts a playback decision for one capture.
// With titles on, release names stay so the ZIP can explain the pick.
// With titles off, each name becomes a neutral token: resolution, source,
// group and size, plus a hash salted by this capture. The same name maps
// to the same token inside the capture, and a later capture does not.
func exportPlayback(p *PlaybackDecision, titles bool, captureID string, uses []secretUse) *PlaybackDecision {
	if p == nil {
		return nil
	}
	out := *p
	out.Profile = redactString(out.Profile, uses)
	out.Chooser = redactString(out.Chooser, uses)
	sizes := map[string]int64{}
	for _, c := range out.Candidates {
		if c.Size > sizes[c.Name] {
			sizes[c.Name] = c.Size
		}
	}
	name := func(raw string) string {
		if raw == "" {
			return ""
		}
		if titles {
			return redactString(raw, uses)
		}
		return redactString(releaseToken(raw, sizes[raw], captureID), uses)
	}
	out.ChosenRelease = name(out.ChosenRelease)
	if len(out.Candidates) > 0 {
		out.Candidates = append([]PlaybackCandidate(nil), out.Candidates...)
		for i := range out.Candidates {
			c := &out.Candidates[i]
			c.Name = name(c.Name)
			c.Reason = redactString(c.Reason, uses)
		}
	}
	if len(out.Variants) > 0 {
		out.Variants = append([]PlaybackVariant(nil), out.Variants...)
		for i := range out.Variants {
			v := &out.Variants[i]
			v.Release = name(v.Release)
		}
	}
	return &out
}

// releaseToken keeps the facts a ranking can read and drops the title.
func releaseToken(name string, size int64, captureID string) string {
	info := releaseparse.Parse(name)
	var parts []string
	if info.Height > 0 {
		parts = append(parts, strconv.Itoa(info.Height)+"p")
	}
	if info.Source != "" {
		parts = append(parts, string(info.Source))
	}
	if info.Group != "" {
		parts = append(parts, info.Group)
	}
	if label := sizeLabel(size); label != "" {
		parts = append(parts, label)
	}
	sum := sha256.Sum256([]byte("ether-support-release:" + captureID + ":" + name))
	hash := hex.EncodeToString(sum[:4])
	if len(parts) == 0 {
		return "#" + hash
	}
	return strings.Join(parts, "/") + " #" + hash
}

func sizeLabel(n int64) string {
	if n <= 0 {
		return ""
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	suffix := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	whole := n / div
	frac := (n % div) * 10 / div
	if frac == 0 {
		return strconv.FormatInt(whole, 10) + suffix
	}
	return strconv.FormatInt(whole, 10) + "." + strconv.FormatInt(frac, 10) + suffix
}

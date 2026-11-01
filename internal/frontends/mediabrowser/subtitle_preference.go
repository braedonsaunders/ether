package mediabrowser

import (
	"errors"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

var errBadSubtitle = errors.New("bad subtitle preference")

// Jellyfin and Emby share UserConfiguration.SubtitleMode. Smart is the
// foreign-audio setting Silo calls "auto".
func jellyfinSubtitleMode(mode string) (string, bool) {
	switch mode {
	case "off":
		return "None", true
	case "forced-only":
		return "OnlyForced", true
	case "always":
		return "Always", true
	case "foreign-only":
		return "Smart", true
	default:
		return "", false
	}
}

func etherSubtitleMode(mode string) (string, bool) {
	switch mode {
	case "None", "Default":
		return "off", true
	case "OnlyForced":
		return "forced-only", true
	case "Always":
		return "always", true
	case "Smart":
		return "foreign-only", true
	default:
		return "", false
	}
}

// mergeSubtitleLanguages keeps an ordered Ether list when a client that only
// has one slot echoes the first language. A different value replaces the list.
// Commas carry more than one language when a client sends them.
func mergeSubtitleLanguages(current []string, posted string) ([]string, error) {
	posted = strings.TrimSpace(posted)
	if posted == "" {
		return []string{}, nil
	}
	if len(posted) > 200 {
		return nil, errBadSubtitle
	}
	parts := strings.Split(posted, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || len(part) < 2 || len(part) > 15 || strings.ContainsAny(part, " /\\\t\r\n") {
			return nil, errBadSubtitle
		}
		key := strings.ToLower(part)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, part)
	}
	if len(out) == 1 && len(current) > 1 && strings.EqualFold(out[0], current[0]) {
		return append([]string(nil), current...), nil
	}
	return out, nil
}

func (i *instance) saveSubtitleConfiguration(r *http.Request, v *frontend.Viewer, object map[string]any) error {
	if i.host.TrackPreferences == nil {
		return errors.New("track preferences unavailable")
	}
	prefs, err := i.host.TrackPreferences.Get(r.Context(), v)
	if err != nil {
		return err
	}
	if raw, ok := object["SubtitleMode"]; ok {
		text, ok := raw.(string)
		mode, known := etherSubtitleMode(text)
		if !ok || !known {
			return errBadSubtitle
		}
		prefs.SubtitleMode = mode
	}
	if raw, ok := object["SubtitleLanguagePreference"]; ok {
		text, ok := raw.(string)
		if !ok {
			return errBadSubtitle
		}
		langs, err := mergeSubtitleLanguages(prefs.SubtitleLanguages, text)
		if err != nil {
			return err
		}
		prefs.SubtitleLanguages = langs
	}
	return i.host.TrackPreferences.SetSubtitle(r.Context(), v, prefs.SubtitleLanguages, prefs.SubtitleMode)
}

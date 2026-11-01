package silo

import (
	"encoding/json"
	"regexp"
)

// validSubtitleAppearance checks the Silo settings object. Unknown keys are
// allowed so the app can round-trip styling this server does not edit.
// Known keys use the same limits as trackprefs.
func validSubtitleAppearance(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" || len(raw) > 4096 {
		return false
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return false
	}
	if v, ok := obj["fontSize"]; ok {
		s, _ := v.(string)
		switch s {
		case "small", "medium", "large":
		default:
			return false
		}
	}
	if v, ok := obj["fontFamily"]; ok {
		s, _ := v.(string)
		switch s {
		case "sans-serif", "serif", "monospace":
		default:
			return false
		}
	}
	for _, key := range []string{"fontColor", "backgroundColor", "textOutlineColor"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		s, _ := v.(string)
		if !subtitleColor.MatchString(s) {
			return false
		}
	}
	if v, ok := obj["backgroundStyle"]; ok {
		s, _ := v.(string)
		switch s {
		case "none", "shadow", "box", "outline":
		default:
			return false
		}
	}
	if v, ok := obj["backgroundOpacity"]; ok {
		n, ok := v.(float64)
		if !ok || n < 0 || n > 100 || n != float64(int(n)) {
			return false
		}
	}
	if v, ok := obj["textOutline"]; ok {
		if _, ok := v.(bool); !ok {
			return false
		}
	}
	if v, ok := obj["position"]; ok {
		s, _ := v.(string)
		switch s {
		case "top", "center", "bottom":
		default:
			return false
		}
	}
	return true
}

var subtitleColor = regexp.MustCompile(`^#[0-9a-fA-F]{3}([0-9a-fA-F]{3})?$`)

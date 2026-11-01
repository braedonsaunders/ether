package trackprefs

import (
	"encoding/json"
	"errors"
	"regexp"
)

// Appearance is optional. An empty document means the client keeps its own
// default styling. Known fields are checked when present; extra keys are kept
// so a Silo client can round-trip fields this server does not edit.
func validateAppearance(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if len(raw) > 4096 {
		return errors.New("invalid subtitle appearance")
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return errors.New("invalid subtitle appearance")
	}
	if v, ok := obj["fontSize"]; ok {
		s, _ := v.(string)
		switch s {
		case "small", "medium", "large":
		default:
			return errors.New("invalid subtitle appearance")
		}
	}
	if v, ok := obj["fontFamily"]; ok {
		s, _ := v.(string)
		switch s {
		case "sans-serif", "serif", "monospace":
		default:
			return errors.New("invalid subtitle appearance")
		}
	}
	for _, key := range []string{"fontColor", "backgroundColor", "textOutlineColor"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		s, _ := v.(string)
		if !subtitleColor.MatchString(s) {
			return errors.New("invalid subtitle appearance")
		}
	}
	if v, ok := obj["backgroundStyle"]; ok {
		s, _ := v.(string)
		switch s {
		case "none", "shadow", "box", "outline":
		default:
			return errors.New("invalid subtitle appearance")
		}
	}
	if v, ok := obj["backgroundOpacity"]; ok {
		n, ok := v.(float64)
		if !ok || n < 0 || n > 100 || n != float64(int(n)) {
			return errors.New("invalid subtitle appearance")
		}
	}
	if v, ok := obj["textOutline"]; ok {
		if _, ok := v.(bool); !ok {
			return errors.New("invalid subtitle appearance")
		}
	}
	if v, ok := obj["position"]; ok {
		s, _ := v.(string)
		switch s {
		case "top", "center", "bottom":
		default:
			return errors.New("invalid subtitle appearance")
		}
	}
	return nil
}

var subtitleColor = regexp.MustCompile(`^#[0-9a-fA-F]{3}([0-9a-fA-F]{3})?$`)

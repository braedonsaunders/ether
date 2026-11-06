package onair

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/onair/scene"
)

var controlToken = regexp.MustCompile(`\{control\.([^{}]+)\}`)

func (s *Service) validateScenes(ctx context.Context, l Look) error {
	controls := map[string]StyleControl{}
	for _, c := range l.Controls {
		if c.ID == "" || controls[c.ID].ID != "" {
			return fmt.Errorf("control %q: choose a unique id", c.ID)
		}
		valid := false
		switch c.Kind {
		case "number":
			v, ok := c.Value.(float64)
			valid = ok && !math.IsNaN(v) && !math.IsInf(v, 0) && (c.Min == nil || v >= *c.Min) && (c.Max == nil || v <= *c.Max)
		case "toggle":
			_, valid = c.Value.(bool)
		case "color", "font", "text", "image":
			_, valid = c.Value.(string)
		}
		if !valid {
			return fmt.Errorf("control %q: value must match its %s type and range", c.Label, c.Kind)
		}
		controls[c.ID] = c
	}
	for role, c := range l.Roles {
		if c.Duration < 4 || c.Duration > 30 || math.Mod(c.Duration, 4) != 0 {
			return fmt.Errorf("%s duration must be a multiple of 4 between 4 and 30 seconds", role)
		}
	}
	ids := map[string]bool{}
	asset := func(d, kind, where string) error {
		if d == "" {
			return nil
		}
		var n int
		query := "SELECT COUNT(*) FROM onair_assets WHERE digest=?"
		args := []any{d}
		if kind != "" {
			query += " AND kind=?"
			args = append(args, kind)
		}
		if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%s: %s asset %q is missing", where, kind, d)
		}
		return nil
	}
	for _, sc := range l.Scenes {
		where := fmt.Sprintf("scene %q", sc.Name)
		if sc.ID == "" || ids[sc.ID] {
			return fmt.Errorf("%s: choose a unique scene id", where)
		}
		ids[sc.ID] = true
		role, ok := l.Roles[sc.Role]
		if !ok || role.Duration != sc.Document.Duration || sc.Role != sc.Document.Moment {
			return fmt.Errorf("%s: document role and duration must match its role", where)
		}
		if sc.Weight < 1 || sc.Weight > 10 {
			return fmt.Errorf("%s: rotation weight must be between 1 and 10", where)
		}
		raw, err := json.Marshal(sc.Document)
		if err != nil {
			return err
		}
		if _, err = scene.Decode(raw); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if len(sc.Sounds) > 4 {
			return fmt.Errorf("%s: use at most four sounds", where)
		}
		soundIDs := map[string]bool{}
		for _, snd := range sc.Sounds {
			if snd.ID == "" || soundIDs[snd.ID] || snd.AssetDigest == "" || snd.At < 0 || snd.At >= role.Duration || snd.Start < 0 || snd.Volume < 0 || snd.Volume > 1 || snd.FadeIn < 0 || snd.FadeOut < 0 || (snd.End != nil && *snd.End <= snd.Start) {
				return fmt.Errorf("%s, sound %q: check id, trim, offset, fades and volume", where, snd.ID)
			}
			soundIDs[snd.ID] = true
			if err = asset(snd.AssetDigest, "audio", where); err != nil {
				return err
			}
		}
		if v := sc.Video; v != nil {
			if v.AssetDigest == "" || (v.Mode != "background" && v.Mode != "replace") || (v.Fit != "cover" && v.Fit != "contain") || v.Start < 0 {
				return fmt.Errorf("%s: choose a video, mode, fit and nonnegative start", where)
			}
			if err = asset(v.AssetDigest, "video", where); err != nil {
				return err
			}
		}
		var walk func([]scene.Layer) error
		walk = func(ls []scene.Layer) error {
			for _, layer := range ls {
				label := layer.Name
				if label == "" {
					label = layer.ID
				}
				at := where + ", layer " + fmt.Sprintf("%q", label)

				var checkValue func(any, string) error
				checkValue = func(v any, field string) error {
					switch x := v.(type) {
					case string:
						for _, m := range controlToken.FindAllStringSubmatch(x, -1) {
							c, ok := controls[m[1]]
							if !ok {
								return fmt.Errorf("%s: control %q does not exist", at, m[1])
							}
							expected := "number"
							switch field {
							case "text":
								expected = "text"
							case "fill", "stroke", "tint", "stops":
								expected = "color"
							case "font":
								expected = "font"
							case "src":
								expected = "image"
							}
							compatible := c.Kind == expected || (expected == "text" && (c.Kind == "number" || c.Kind == "toggle"))
							if !compatible {
								return fmt.Errorf("%s: %s needs a %s control; %q is %s", at, field, expected, c.Label, c.Kind)
							}
						}
					case []any:
						for _, child := range x {
							if err := checkValue(child, field); err != nil {
								return err
							}
						}
					case map[string]any:
						for key, child := range x {
							next := field
							if key != "keys" && key != "linear" && key != "radial" {
								next = key
							}
							if err := checkValue(child, next); err != nil {
								return err
							}
						}
					}
					return nil
				}
				raw, _ := json.Marshal(layer)
				var values map[string]any
				_ = json.Unmarshal(raw, &values)
				delete(values, "layers")
				for field, value := range values {
					if err := checkValue(value, field); err != nil {
						return err
					}
				}

				for _, v := range []any{layer.Src, layer.Font} {
					if d, ok := v.(string); ok {
						if strings.HasPrefix(d, "{control.") {
							c := controls[strings.TrimSuffix(strings.TrimPrefix(d, "{control."), "}")]
							if x, ok := c.Value.(string); ok {
								d = x
							}
						}
						if d == "{brand.logo}" {
							d = l.Brand.Logo
						}
						if len(d) == 64 {
							if err := asset(d, "", at); err != nil {
								return err
							}
						}
					}
				}

				tokens := map[string]any{"brand.logo": l.Brand.Logo, "type.display": l.Brand.Type.Display, "type.body": l.Brand.Type.Body}
				for _, control := range l.Controls {
					tokens["control."+control.ID] = control.Value
				}
				own := layer
				own.Layers = nil
				refs := map[string]string{}
				collectSceneAssets([]scene.Layer{own}, scene.Bindings{Tokens: tokens, Slots: map[string]string{}}, refs)
				for digest := range refs {
					if digest == l.Brand.Logo && utf8.RuneCountInString(digest) <= 3 {
						continue
					}
					if err := asset(digest, "", at); err != nil {
						return err
					}
				}
				if err := walk(layer.Layers); err != nil {
					return err
				}
			}
			return nil
		}
		if err = walk(sc.Document.Layers); err != nil {
			return err
		}
	}
	if utf8.RuneCountInString(l.Brand.Logo) > 3 && l.Brand.Logo != "" {
		if err := asset(l.Brand.Logo, "image", "brand logo"); err != nil {
			return err
		}
	}
	return nil
}

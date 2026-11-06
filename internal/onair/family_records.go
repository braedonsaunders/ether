package onair

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/onair/families"
)

type TemplateRecord struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Blurb    string   `json:"blurb"`
	BlurbKey string   `json:"blurbKey"`
	Tags     []string `json:"tags"`
	Style    Look     `json:"style"`
}

func familyLook(id, name string) Look {
	if id == "lateNight" {
		id = "late-night"
	}
	b := families.Defaults(id)
	l := Look{Name: name, Template: id, Roles: map[string]RoleConfig{}, Scenes: []StyleScene{}, Controls: []StyleControl{}, ChannelIDs: []string{}}
	str := func(k string) string { return fmt.Sprint(b.Tokens[k]) }
	l.Brand.Palette.Background = str("bg")
	l.Brand.Palette.Accent = str("accent")
	l.Brand.Palette.Text = str("text")
	l.Brand.Palette.Glow = str("glow")
	l.Brand.Type.Display = str("type.display")
	l.Brand.Type.Body = str("type.mono")
	l.Brand.Energy = .5
	l.Brand.Textures = []string{"grain"}
	l.Brand.Language = "en"
	l.Brand.Tagline = str("words.tagline")
	if raw, err := families.Controls(id); err == nil {
		_ = json.Unmarshal(raw, &l.Controls)
	}
	for _, m := range families.Names()[id] {
		if strings.HasPrefix(m, "screen.") {
			continue
		}
		d, err := families.Get(id, m)
		if err != nil {
			continue
		}
		// Bake copy into all values, including animated text keys.
		raw, _ := json.Marshal(d)
		var value any
		_ = json.Unmarshal(raw, &value)
		var bake func(any) any
		bake = func(v any) any {
			switch x := v.(type) {
			case string:
				x = strings.ReplaceAll(x, "{type.mono}", "{type.body}")
				for k, t := range b.Tokens {
					if strings.HasPrefix(k, "words.") || k == "moon" || k == "fog" {
						x = strings.ReplaceAll(x, "{"+k+"}", fmt.Sprint(t))
					}
				}
				return x
			case []any:
				for i := range x {
					x[i] = bake(x[i])
				}
				return x
			case map[string]any:
				for k := range x {
					x[k] = bake(x[k])
				}
				return x
			}
			return v
		}
		raw, _ = json.Marshal(bake(value))
		_ = json.Unmarshal(raw, &d)
		duration := float64(8)
		d.Duration = duration
		if d.Poster >= duration {
			d.Poster = duration / 2
		}
		l.Roles[m] = RoleConfig{Duration: duration}
		sounds := []SceneSound{}
		if sting, ok := b.Tokens["sting"].(string); ok && sting != "" && m == "ident" {
			sounds = append(sounds, SceneSound{ID: "sting", AssetDigest: sting, Volume: .6})
		}
		sceneName := map[string]string{"ident": name + " ident", "upnext": "Up next card", "tonight": "Tonight lineup", "break": "Break bumper", "back": "Back from break", "offair": "Off air card", "welcome": "Welcome card", "lowerthird": "Programme lower third", "bug": "Corner logo"}[m]
		if sceneName == "" {
			sceneName = name + " " + m
		}
		l.Scenes = append(l.Scenes, StyleScene{ID: id + "-" + m, Role: m, Name: sceneName, Enabled: true, Weight: 1, Document: d, Sounds: sounds})
	}
	return l
}
func (s *Service) Templates(_ context.Context) ([]TemplateRecord, error) {
	names := families.Names()
	ids := make([]string, 0, len(names))
	for id := range names {
		if id != "signal" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := []TemplateRecord{}
	for _, id := range ids {
		name, blurb, tags := families.Description(id)
		out = append(out, TemplateRecord{ID: id, Name: name, Blurb: blurb, BlurbKey: "onair.template." + id + ".blurb", Tags: tags, Style: familyLook(id, name)})
	}
	return out, nil
}

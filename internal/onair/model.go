// Package onair owns owner looks, assets, reels and background master clips.
package onair

import (
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"time"
)

type Brand struct {
	Palette struct {
		Background string `json:"background"`
		Accent     string `json:"accent"`
		Text       string `json:"text"`
		Glow       string `json:"glow"`
	} `json:"palette"`
	Type struct {
		Display string `json:"display"`
		Body    string `json:"body"`
	} `json:"type"`
	Energy   float64  `json:"energy"`
	Textures []string `json:"textures"`
	Tagline  string   `json:"tagline"`
	Language string   `json:"language"`
	Logo     string   `json:"logo,omitempty"`
}
type RoleConfig struct {
	Duration float64 `json:"duration"`
}
type SceneSound struct {
	ID          string   `json:"id"`
	AssetDigest string   `json:"assetDigest"`
	At          float64  `json:"at"`
	Start       float64  `json:"start"`
	End         *float64 `json:"end,omitempty"`
	Volume      float64  `json:"volume"`
	FadeIn      float64  `json:"fadeIn"`
	FadeOut     float64  `json:"fadeOut"`
}
type SceneVideo struct {
	AssetDigest string  `json:"assetDigest"`
	Mode        string  `json:"mode"`
	Fit         string  `json:"fit"`
	Start       float64 `json:"start"`
	Crossfade   bool    `json:"crossfade,omitempty"`
}
type StyleScene struct {
	ID       string         `json:"id"`
	Role     string         `json:"role"`
	Name     string         `json:"name"`
	Enabled  bool           `json:"enabled"`
	Weight   int            `json:"weight"`
	Document scene.Document `json:"document"`
	Sounds   []SceneSound   `json:"sounds"`
	Video    *SceneVideo    `json:"video,omitempty"`
}
type StyleControl struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Kind  string   `json:"kind"`
	Value any      `json:"value"`
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
	Step  *float64 `json:"step,omitempty"`
	Group string   `json:"group,omitempty"`
}
type Look struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Template     string                `json:"template"`
	Brand        Brand                 `json:"brand"`
	Roles        map[string]RoleConfig `json:"roles"`
	Scenes       []StyleScene          `json:"scenes"`
	Controls     []StyleControl        `json:"controls"`
	ChannelIDs   []string              `json:"channelIds"`
	ChannelCount int                   `json:"channelCount"`
	Bug          *core.BugSettings     `json:"bug,omitempty"`
	Screens      *config.LiveTVScreens `json:"screens,omitempty"`
	Pack         *Pack                 `json:"pack,omitempty"`
	Render       *Progress             `json:"render,omitempty"`
}
type Pack struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Progress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}
type Asset struct {
	Source      string   `json:"source,omitempty"`
	Attribution string   `json:"attribution,omitempty"`
	Status      string   `json:"status,omitempty"`
	Progress    float64  `json:"progress,omitempty"`
	Error       string   `json:"error,omitempty"`
	Digest      string   `json:"digest"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Size        int64    `json:"size"`
	License     string   `json:"license,omitempty"`
	UsedBy      []string `json:"usedBy"`
	URL         string   `json:"url,omitempty"`
	MIME        string   `json:"-"`
	DurationMS  int64    `json:"durationMs,omitempty"`
	Width       int      `json:"width,omitempty"`
	Height      int      `json:"height,omitempty"`
}
type Reel struct {
	Anchor        time.Time  `json:"-"`
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Order         string     `json:"order"`
	RepeatHours   float64    `json:"repeatHours"`
	LoudnessMatch bool       `json:"loudnessMatch"`
	TrimSilence   bool       `json:"trimSilence"`
	Clips         []ReelClip `json:"clips"`
	ChannelIDs    []string   `json:"channelIds"`
}
type ReelClip struct {
	Source       string  `json:"source,omitempty"`
	Attribution  string  `json:"attribution,omitempty"`
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	DurationMS   int64   `json:"durationMs"`
	SourceDigest string  `json:"sourceDigest,omitempty"`
	Status       string  `json:"status"`
	Progress     float64 `json:"progress,omitempty"`
	Size         int64   `json:"size,omitempty"`
	Preview      string  `json:"preview,omitempty"`
	Error        string  `json:"error,omitempty"`
}

// Every owned document goes through the strict, cost-limited scene decoder.
func (s *StyleScene) UnmarshalJSON(b []byte) error {
	type plain StyleScene
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	var raw struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	d, err := scene.Decode(raw.Document)
	if err != nil {
		return fmt.Errorf("scene %q: %w", v.Name, err)
	}
	v.Document = d
	*s = StyleScene(v)
	return nil
}

// emptyCollections keeps a stored style's lists non-nil, so the API always
// returns [] and {} instead of null.
func (l *Look) emptyCollections() {
	if l.Scenes == nil {
		l.Scenes = []StyleScene{}
	}
	if l.Controls == nil {
		l.Controls = []StyleControl{}
	}
	if l.Roles == nil {
		l.Roles = map[string]RoleConfig{}
	}
	if l.ChannelIDs == nil {
		l.ChannelIDs = []string{}
	}
}

package core

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// ClipRequest binds a look moment to the channel and upcoming programme.
type ClipRequest struct {
	// AssetDigest overrides this channel's moment without changing its shared look.
	AssetDigest   string        `json:"assetDigest,omitempty"`
	AssetDuration time.Duration `json:"assetDuration,omitempty"`
	Bug           *BugSettings

	LookID    string
	Moment    string
	ChannelID string
	ViewerID  int64
	Bind      map[string]string
	Art       map[string]string
	Language  string
}
type ClipRef struct {
	Hash     string
	Duration time.Duration
	Path     string
}

// ChannelGraphics never renders synchronously. Clip only reads the local cache.
type ChannelGraphics interface {
	Duration(lookID, moment string) (time.Duration, bool)
	Clip(context.Context, ClipRequest) (ClipRef, bool)
	Ensure(context.Context, ClipRequest)
}

// PersonalPick is a viewer-safe programming candidate with its explanation.
type PersonalPick struct {
	Title  *Title
	Reason string
	Kind   string
	Score  float64
}

// PersonalPicker supplies background personal-channel rundown builders.
type PersonalPicker interface {
	Picks(context.Context, *User, map[string]float64, int) ([]PersonalPick, error)
}

// ChannelSegmentMetadata adds plugin policy metadata without expanding base fakes.
type ChannelSegmentMetadata interface {
	SegmentInfo(moment string) (name, rating string, ok bool)
}

// BugSettings is a sparse owner override; nil fields inherit the look defaults.
type BugSettings struct {
	Position    string   `json:"position,omitempty"`
	Opacity     *float64 `json:"opacity,omitempty"`
	Style       string   `json:"style,omitempty"`
	Size        string   `json:"size,omitempty"`
	Source      string   `json:"source,omitempty"`
	FadeBreaks  *bool    `json:"fadeBreaks,omitempty"`
	FadeCredits *bool    `json:"fadeCredits,omitempty"`
}

func ResolveBug(base, override *BugSettings) BugSettings {
	opacity, fades, credits := .30, true, false
	b := BugSettings{Position: "bottomRight", Opacity: &opacity, Style: "monochrome", Size: "medium", Source: "logo", FadeBreaks: &fades, FadeCredits: &credits}
	for _, v := range []*BugSettings{base, override} {
		if v == nil {
			continue
		}
		if v.Position != "" {
			b.Position = v.Position
		}
		if v.Opacity != nil {
			b.Opacity = v.Opacity
		}
		if v.Style != "" {
			b.Style = v.Style
		}
		if v.Size != "" {
			b.Size = v.Size
		}
		if v.Source != "" {
			b.Source = v.Source
		}
		if v.FadeBreaks != nil {
			b.FadeBreaks = v.FadeBreaks
		}
		if v.FadeCredits != nil {
			b.FadeCredits = v.FadeCredits
		}
	}
	return b
}

func (b *BugSettings) Validate() error {
	if b == nil {
		return nil
	}
	for _, pair := range [][2]string{{b.Position, "topLeft topRight bottomLeft bottomRight"}, {b.Style, "monochrome original"}, {b.Size, "small medium large"}, {b.Source, "logo wordmark"}} {
		if pair[0] != "" && !slices.Contains(strings.Fields(pair[1]), pair[0]) {
			return fmt.Errorf("invalid corner logo setting")
		}
	}
	if b.Opacity != nil && (math.IsNaN(*b.Opacity) || *b.Opacity < .15 || *b.Opacity > .6) {
		return fmt.Errorf("corner logo opacity must be 15–60%%")
	}
	return nil
}

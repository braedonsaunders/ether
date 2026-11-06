// Package scene defines the portable, executable-code-free On Air document v1.
package scene

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

type Value = any

type Document struct {
	Audio    *Audio   `json:"audio,omitempty"`
	V        int      `json:"v"`
	Moment   string   `json:"moment"`
	Duration float64  `json:"duration"`
	Poster   float64  `json:"poster"`
	Slots    []string `json:"slots,omitempty"`
	Layers   []Layer  `json:"layers"`
	Post     []Effect `json:"post,omitempty"`
}
type Audio struct {
	Generator string `json:"generator"`
	Seed      uint32 `json:"seed,omitempty"`
}

type Layer struct {
	Name        string     `json:"name,omitempty"`
	Hidden      bool       `json:"hidden,omitempty"`
	Locked      bool       `json:"locked,omitempty"`
	Start       float64    `json:"start,omitempty"`
	End         float64    `json:"end,omitempty"`
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	X           Value      `json:"x,omitempty"`
	Y           Value      `json:"y,omitempty"`
	W           Value      `json:"w,omitempty"`
	H           Value      `json:"h,omitempty"`
	Scale       Value      `json:"scale,omitempty"`
	ScaleX      Value      `json:"scaleX,omitempty"`
	ScaleY      Value      `json:"scaleY,omitempty"`
	Rotate      Value      `json:"rotate,omitempty"`
	Opacity     Value      `json:"opacity,omitempty"`
	Fill        Value      `json:"fill,omitempty"`
	Stroke      Value      `json:"stroke,omitempty"`
	StrokeWidth Value      `json:"strokeWidth,omitempty"`
	Join        string     `json:"join,omitempty"`
	Cap         string     `json:"cap,omitempty"`
	FillRule    string     `json:"fillRule,omitempty"`
	Path        string     `json:"path,omitempty"`
	Clip        string     `json:"clip,omitempty"`
	Text        Value      `json:"text,omitempty"`
	Font        Value      `json:"font,omitempty"`
	Size        Value      `json:"size,omitempty"`
	Weight      int        `json:"weight,omitempty"`
	Track       Value      `json:"track,omitempty"`
	Align       string     `json:"align,omitempty"`
	MaxWidth    float64    `json:"maxWidth,omitempty"`
	Fit         string     `json:"fit,omitempty"`
	MaxLines    int        `json:"maxLines,omitempty"`
	Src         Value      `json:"src,omitempty"`
	Tint        Value      `json:"tint,omitempty"`
	TintBlend   string     `json:"tintBlend,omitempty"`
	Blend       string     `json:"blend,omitempty"`
	Blur        float64    `json:"blur,omitempty"`
	Glow        float64    `json:"glow,omitempty"`
	Count       int        `json:"count,omitempty"`
	Seed        uint32     `json:"seed,omitempty"`
	Repeat      int        `json:"repeat,omitempty"`
	Behave      []Behavior `json:"behave,omitempty"`
	Reveal      *Reveal    `json:"reveal,omitempty"`
	Layers      []Layer    `json:"layers,omitempty"`
}
type Reveal struct {
	Out  bool    `json:"out,omitempty"`
	Wipe string  `json:"wipe"`
	From float64 `json:"from"`
	To   float64 `json:"to"`
	Ease string  `json:"ease,omitempty"`
}
type Params struct {
	Start     float64 `json:"start,omitempty"`
	Step      float64 `json:"step,omitempty"`
	Amplitude float64 `json:"amplitude,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
	Seed      uint32  `json:"seed,omitempty"`
}
type Behavior struct {
	Typewriter *Params `json:"typewriter,omitempty"`
	Flicker    *Params `json:"flicker,omitempty"`
	Wiggle     *Params `json:"wiggle,omitempty"`
	Bounce     *Params `json:"bounce,omitempty"`
	Glint      *Params `json:"glint,omitempty"`
	Chroma     *Params `json:"chroma,omitempty"`
	Drift      *Params `json:"drift,omitempty"`
	Pop        *Params `json:"pop,omitempty"`
	Squash     *Params `json:"squash,omitempty"`
}
type Effect struct {
	Type   string `json:"type"`
	Amount Value  `json:"amount"`
	Seed   uint32 `json:"seed,omitempty"`
}
type Bindings struct {
	Tokens map[string]any    `json:"tokens,omitempty"`
	Slots  map[string]string `json:"slots,omitempty"`
}
type Cost struct {
	Layers    int
	Particles int
	Blur      float64
	Duration  float64
}

func Decode(data []byte) (Document, error) {
	var d Document
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return d, fmt.Errorf("trailing JSON")
	}
	return d, d.Validate()
}
func (d Document) Estimate() Cost {
	c := Cost{Duration: d.Duration}
	var visit func([]Layer, int)
	visit = func(ls []Layer, m int) {
		for _, l := range ls {
			n := max(1, l.Repeat) * m
			c.Layers += n
			c.Particles += l.Count * n
			c.Blur = max(c.Blur, l.Blur, l.Glow)
			visit(l.Layers, n)
		}
	}
	visit(d.Layers, 1)
	return c
}
func (d Document) Validate() error {
	if d.V != 1 || (!strings.HasPrefix(d.Moment, "plugin:") && !one(d.Moment, "ident upnext tonight break back offair welcome bug lowerthird screen.card screen.static screen.bars screen.brb screen.testcard")) {
		return fmt.Errorf("unsupported scene version or moment")
	}
	maxDuration := 30.0
	if strings.HasPrefix(d.Moment, "plugin:") {
		maxDuration = 600
	}
	if d.Duration <= 0 || d.Duration > maxDuration || math.Mod(d.Duration, 4) != 0 || d.Poster < 0 || d.Poster >= d.Duration {
		return fmt.Errorf("invalid duration/poster: duration must be a multiple of 4 within the moment limit")
	}
	if d.Audio != nil && !one(d.Audio.Generator, "static tone bed") {
		return fmt.Errorf("unknown audio generator")
	}
	ids := map[string]bool{}
	total := 0
	particles := 0
	var visit func([]Layer, int, int) error
	visit = func(ls []Layer, depth, m int) error {
		if depth > 16 {
			return fmt.Errorf("group depth exceeds 16")
		}
		for _, l := range ls {
			if l.ID == "" || ids[l.ID] {
				return fmt.Errorf("missing/duplicate layer id %q", l.ID)
			}
			ids[l.ID] = true
			if math.IsNaN(l.Start) || math.IsNaN(l.End) || l.Start < 0 || l.End < 0 || l.Start > d.Duration || l.End > d.Duration || (l.End > 0 && l.End <= l.Start) {
				return fmt.Errorf("layer %q: start and end must be within the scene, with end after start", l.ID)
			}
			if !one(l.Type, "rect ellipse path text image art logo blob particles gradient noise grid group") {
				return fmt.Errorf("layer %q: unknown type %q", l.ID, l.Type)
			}
			if l.Repeat < 0 || l.Repeat > 200 || l.Count < 0 || l.Count > 2000 {
				return fmt.Errorf("layer %q: repeat must be within 0..200 and particles within 0..2000", l.ID)
			}
			n := max(1, l.Repeat) * m
			total += n
			particles += l.Count * n
			if total > 200 || particles > 2000 || l.Blur < 0 || l.Blur > 64 || l.Glow < 0 || l.Glow > 64 {
				return fmt.Errorf("layer %q: scene exceeds render cost limit", l.ID)
			}
			if !one(l.Blend, " normal multiply screen overlay") || !one(l.TintBlend, " normal multiply screen overlay silhouette") || !one(l.Fit, " shrink wrap clip cover contain") || !one(l.FillRule, " nonzero evenodd") || !one(l.Join, " miter round bevel") || !one(l.Cap, " butt round square") || l.MaxLines < 0 || l.MaxLines > 100 {
				return fmt.Errorf("layer %q: invalid blend, fit, fill, stroke or line limit", l.ID)
			}
			for _, v := range []Value{l.X, l.Y, l.W, l.H, l.Scale, l.ScaleX, l.ScaleY, l.Rotate, l.Opacity, l.Fill, l.Stroke, l.StrokeWidth, l.Text, l.Font, l.Size, l.Track, l.Src, l.Tint} {
				if err := validateValue(v); err != nil {
					return fmt.Errorf("layer %q: %w", l.ID, err)
				}
			}
			if l.Reveal != nil && (!one(l.Reveal.Wipe, "ltr rtl") || l.Reveal.To <= l.Reveal.From || !one(l.Reveal.Ease, " linear in out inOut back elastic spring")) {
				return fmt.Errorf("layer %q: invalid reveal direction or timing", l.ID)
			}
			for _, b := range l.Behave {
				raw, _ := json.Marshal(b)
				var fields map[string]any
				_ = json.Unmarshal(raw, &fields)
				if len(fields) != 1 {
					return fmt.Errorf("behaviour requires exactly one kind")
				}
				for _, p := range []*Params{b.Typewriter, b.Flicker, b.Wiggle, b.Bounce, b.Glint, b.Chroma, b.Drift, b.Pop, b.Squash} {
					if p != nil && (p.Step < 0 || p.Speed < 0) {
						return fmt.Errorf("layer %q: behaviour timing must be nonnegative", l.ID)
					}
				}
			}
			if err := visit(l.Layers, depth+1, n); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(d.Layers, 0, 1); err != nil {
		return err
	}
	for _, p := range d.Post {
		if !one(p.Type, "grain scanlines vignette flicker") {
			return fmt.Errorf("unknown post effect")
		}
		if err := validateValue(p.Amount); err != nil {
			return err
		}
	}
	return nil
}
func one(s, allowed string) bool { return strings.Contains(" "+allowed+" ", " "+s+" ") }
func validateValue(v any) error {
	switch x := v.(type) {
	case nil, string, int, uint32:
		return nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("non-finite property")
		}
		return nil
	case map[string]any:
		if keys, ok := x["keys"]; ok {
			if len(x) != 1 {
				return fmt.Errorf("unknown keyframe field")
			}
			arr, ok := keys.([]any)
			if !ok || len(arr) == 0 || len(arr) > 256 {
				return fmt.Errorf("invalid keys")
			}
			prev := -1.0
			for _, k := range arr {
				a, ok := k.([]any)
				if !ok || len(a) < 2 || len(a) > 3 {
					return fmt.Errorf("invalid key")
				}
				t, ok := a[0].(float64)
				if !ok || t < prev || t < 0 || t > 30 {
					return fmt.Errorf("unordered key times")
				}
				prev = t
				if _, ok := a[1].(map[string]any); ok {
					return fmt.Errorf("nested keyframes/paint are invalid key values")
				}
				if err := validateValue(a[1]); err != nil {
					return err
				}
				if len(a) == 3 && !one(fmt.Sprint(a[2]), "linear in out inOut back elastic spring") {
					return fmt.Errorf("unknown ease")
				}
			}
			return nil
		}
		for k := range x {
			if k != "linear" && k != "radial" && k != "stops" {
				return fmt.Errorf("unknown paint field %q", k)
			}
		}

		coords, linear := x["linear"].([]any)
		radialCoords, radial := x["radial"].([]any)
		if linear == radial {
			return fmt.Errorf("gradient needs exactly one coordinate kind")
		}
		if radial {
			coords = radialCoords
		}
		if len(coords) != 4 {
			return fmt.Errorf("gradient needs four coordinates")
		}
		for _, v := range coords {
			if _, ok := v.(float64); !ok {
				return fmt.Errorf("gradient coordinate must be numeric")
			}
		}
		stops, ok := x["stops"].([]any)
		if !ok || len(stops) < 2 || len(stops) > 64 {
			return fmt.Errorf("invalid gradient stops")
		}
		prev := -1.0
		for _, v := range stops {
			a, ok := v.([]any)
			if !ok || len(a) != 2 {
				return fmt.Errorf("invalid gradient stop")
			}
			at, ok := a[0].(float64)
			if !ok || at < prev || at < 0 || at > 1 {
				return fmt.Errorf("unordered gradient stops")
			}
			if _, ok := a[1].(string); !ok {
				return fmt.Errorf("gradient colour must be a string")
			}
			prev = at
		}
		return nil
	}
	return fmt.Errorf("invalid property value")
}
func (b Bindings) Resolve(s string) string {
	for i := 0; i < 16; i++ {
		start := strings.Index(s, "{")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			break
		}
		end += start
		key := s[start+1 : end]
		v, ok := b.Slots[key]
		if !ok {
			a := lookup(b.Tokens, key)
			if a != nil {
				v = fmt.Sprint(a)
			}
		}
		s = s[:start] + v + s[end+1:]
	}
	return s
}
func lookup(m map[string]any, k string) any {
	if v, ok := m[k]; ok {
		return v
	}
	parts := strings.SplitN(k, ".", 2)
	if len(parts) == 2 {
		if child, ok := m[parts[0]].(map[string]any); ok {
			return lookup(child, parts[1])
		}
	}
	return nil
}
func (b Bindings) Number(k string, fallback float64) float64 {
	v := lookup(b.Tokens, k)
	if v == nil {
		return fallback
	}
	return Number(v, fallback)
}
func Number(v any, fallback float64) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		f, e := strconv.ParseFloat(x, 64)
		if e == nil {
			return f
		}
	}
	return fallback
}
func Eval(v Value, b Bindings, t float64) any {
	if s, ok := v.(string); ok {
		return b.Resolve(s)
	}
	if m, ok := v.(map[string]any); ok {
		if a, ok := m["keys"].([]any); ok && len(a) > 0 {
			prev := a[0].([]any)
			for _, k := range a[1:] {
				next := k.([]any)
				if t < Number(next[0], 0) {
					u := (t - Number(prev[0], 0)) / max(.00001, Number(next[0], 0)-Number(prev[0], 0))
					e := "linear"
					if len(next) > 2 {
						e = fmt.Sprint(next[2])
					}
					u = Ease(e, u, b.Number("energy", 1))
					a, z := Eval(prev[1], b, t), Eval(next[1], b, t)
					if as, ok := a.(string); ok {
						if zs, ok := z.(string); ok {
							if c, ok := interpolateColor(as, zs, u); ok {
								return c
							}
							if u >= 1 {
								return zs
							}
							return as
						}
					}
					return Number(a, 0) + (Number(z, 0)-Number(a, 0))*u
				}
				prev = next
			}
			return Eval(prev[1], b, t)
		}
	}
	return v
}
func Ease(name string, u, energy float64) float64 {
	u = max(0, min(1, u))
	switch name {
	case "in":
		return u * u * u
	case "out":
		return 1 - math.Pow(1-u, 3)
	case "inOut":
		if u < .5 {
			return 4 * u * u * u
		}
		return 1 - math.Pow(-2*u+2, 3)/2
	case "back":
		c := 1.70158 * energy
		return 1 + (c+1)*math.Pow(u-1, 3) + c*math.Pow(u-1, 2)
	case "elastic", "spring":
		if u == 0 || u == 1 {
			return u
		}
		return 1 - math.Pow(2, -10*u)*math.Cos(u*math.Pi*6)*energy
	}
	return u
}

func interpolateColor(a, b string, u float64) (string, bool) {
	parse := func(s string) ([4]float64, bool) {
		out := [4]float64{0, 0, 0, 255}
		alpha := 1.0
		if at := strings.LastIndex(s, "@"); at >= 0 {
			var e error
			alpha, e = strconv.ParseFloat(s[at+1:], 64)
			if e != nil {
				return out, false
			}
			s = s[:at]
		}
		if !strings.HasPrefix(s, "#") {
			return out, false
		}
		s = s[1:]
		if len(s) != 6 && len(s) != 8 {
			return out, false
		}
		v, e := strconv.ParseUint(s, 16, 32)
		if e != nil {
			return out, false
		}
		if len(s) == 8 {
			alpha *= float64(v&255) / 255
			v >>= 8
		}
		out = [4]float64{float64(v>>16&255) * alpha, float64(v>>8&255) * alpha, float64(v&255) * alpha, 255 * alpha}
		return out, true
	}
	x, ok := parse(a)
	if !ok {
		return "", false
	}
	y, ok := parse(b)
	if !ok {
		return "", false
	}
	var z [4]float64
	for i := range z {
		z[i] = max(0, min(255, x[i]+(y[i]-x[i])*u))
	}
	alpha := z[3] / 255
	if alpha > 0 {
		for i := 0; i < 3; i++ {
			z[i] /= alpha
		}
	}
	return fmt.Sprintf("#%02x%02x%02x%02x", int(z[0]+.5), int(z[1]+.5), int(z[2]+.5), int(z[3]+.5)), true
}

// StaticForeground separates a full-frame analogue background from its prepared
// foreground. Such scenes air over a live Signal engine instead of a clip loop.
func (d Document) StaticForeground() (Document, bool) {
	if len(d.Layers) == 0 || len(d.Post) != 0 {
		return d, false
	}
	l := d.Layers[0]
	if l.Type != "noise" || l.Blend != "" && l.Blend != "normal" || l.Clip != "" || l.Repeat > 1 || len(l.Behave) > 0 || l.Blur != 0 || l.Glow != 0 {
		return d, false
	}
	for _, v := range []struct {
		value Value
		want  float64
	}{{l.W, 1920}, {l.H, 1080}, {l.X, 0}, {l.Y, 0}, {l.Scale, 1}, {l.ScaleX, 1}, {l.ScaleY, 1}, {l.Rotate, 0}, {l.Opacity, 1}} {
		if v.value != nil && Number(v.value, math.NaN()) != v.want {
			return d, false
		}
	}
	// Only fixed foreground geometry can be prepared once. Procedural layers,
	// reveals and keyframes must keep their ordinary scene animation.
	var fixed func([]Layer) bool
	fixed = func(layers []Layer) bool {
		for _, l := range layers {
			if one(l.Type, "noise blob particles grid") || len(l.Behave) > 0 || l.Reveal != nil {
				return false
			}
			for _, v := range []Value{l.X, l.Y, l.W, l.H, l.Scale, l.ScaleX, l.ScaleY, l.Rotate, l.Opacity, l.Fill, l.Stroke, l.StrokeWidth, l.Text, l.Font, l.Size, l.Track, l.Src, l.Tint} {
				if m, ok := v.(map[string]any); ok {
					if _, animated := m["keys"]; animated {
						return false
					}
				}
			}
			if !fixed(l.Layers) {
				return false
			}
		}
		return true
	}
	if !fixed(d.Layers[1:]) {
		return d, false
	}
	out := d
	out.Layers = append([]Layer(nil), d.Layers[1:]...)
	return out, true
}

package onair

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"strings"
)

// Chatter is the owner's configured model. Proposals never save records.
type Chatter interface {
	Enabled() bool
	Chat(context.Context, []llm.Message, bool) (string, error)
}

func (s *Service) Draft(ctx context.Context, l Look) (Look, error) {
	l.Pack = nil
	l.Render = nil
	for i := range l.Scenes {
		b, e := json.Marshal(l.Scenes[i].Document)
		if e != nil {
			return Look{}, e
		}
		d, e := scene.Decode(b)
		if e != nil {
			return Look{}, fmt.Errorf("scene %q: %w", l.Scenes[i].Name, e)
		}
		l.Scenes[i].Document = d
	}
	return l, s.validateLook(ctx, l)
}

const lookGuide = `Return a complete Style JSON object, without prose or markdown. A style owns every scene document; do not return palette-only edits. Preserve IDs, channelIds and all unrelated content. Shape: {id,name,template,brand:{palette:{background,accent,text,glow},type:{display,body},logo,tagline,energy:0..1,textures:[grain|flicker|scanlines|vignette|chroma],language},roles:{ident:{duration:8},upnext:{duration:8},break:{duration:8},back:{duration:8}},scenes:[{id,role,name,enabled,weight:1..10,document:{v:1,moment,duration,poster,layers:[],post:[]},sounds:[{id,assetDigest,at,start,end,volume,fadeIn,fadeOut}],video?:{assetDigest,mode:background|replace,fit:cover|contain,start}}],controls:[{id,label,kind:color|font|text|number|toggle|image,value,min?,max?,step?,group?}],channelIds:[]}. Role durations are multiples of 4 between 4 and 28; each document duration equals its role. Every layer needs unique id and human name. Layer types: group (layers), rect, ellipse, line, path (SVG path), text (text,font,size,align,maxWidth,fit), image (src), grid, particles, noise. Common fields: x,y,w,h,fill,stroke,strokeWidth,opacity,rotate,scale,start,end,hidden,locked. Animatable values are scalars or {keys:[[seconds,value,easing],...]}; easing linear,in,out,inOut,back. Colours #RRGGBB or tokens; gradients {linear:[x1,y1,x2,y2],stops:[[0,color],[1,color]]}. Behaviours [{bounce:{amplitude,speed}},{wiggle:{amplitude,speed}},{glint:{amplitude,speed}}]. Use tokens {bg},{accent},{text},{glow},{type.display},{type.body},{energy},{textures.grain},{brand.logo},{words.tagline},{control.id}. Live slots {channel.name},{next.title},{next.time},{next.poster},{now.title},{viewer.name}. Never invent titles or asset digests. Sound/video/image/font references may use only the supplied Library assets. Text must contrast its background. Include full layers, considered motion and meaningful grouped controls. The two example documents below demonstrate the renderer's exact grammar.`

func (s *Service) Generate(ctx context.Context, prompt, summary string, base Look) (Look, error) {
	return s.GenerateScene(ctx, prompt, summary, base, "")
}
func (s *Service) GenerateScene(ctx context.Context, prompt, summary string, base Look, sceneID string) (Look, error) {
	if s.o.Chat == nil || !s.o.Chat.Enabled() {
		return Look{}, fmt.Errorf("connect a language model in Integrations to describe a style")
	}
	if len(strings.TrimSpace(prompt)) == 0 || len(prompt) > 2000 {
		return Look{}, fmt.Errorf("prompt must be between 1 and 2000 characters")
	}
	if len(base.Scenes) == 0 {
		base = familyLook("clean", "New style")
	}
	if sceneID != "" {
		found := false
		for _, sc := range base.Scenes {
			if sc.ID == sceneID {
				found = true
			}
		}
		if !found {
			return Look{}, fmt.Errorf("scene does not exist")
		}
	}
	guide := lookGuide
	for _, f := range []string{"clean", "retro"} {
		d, e := families.Get(f, "ident")
		if e != nil {
			return Look{}, e
		}
		b, _ := json.Marshal(d)
		guide += "\nExample: " + string(b)
	}
	assets, e := s.Assets(ctx)
	if e != nil {
		return Look{}, e
	}
	ab, _ := json.Marshal(assets)
	b, _ := json.Marshal(base)
	msgs := []llm.Message{{Role: "system", Content: guide}, {Role: "user", Content: fmt.Sprintf("Request: %s\nChannel summary: %s\nLibrary: %s\nBase style: %s\nOnly change scene ID (empty means whole style): %s", prompt, summary, ab, b, sceneID)}}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		raw, e := s.o.Chat.Chat(ctx, msgs, true)
		if e != nil {
			return Look{}, e
		}
		var l Look
		err = json.Unmarshal([]byte(strings.TrimSpace(raw)), &l)
		if err == nil {
			l.ID = base.ID
			l.ChannelIDs = base.ChannelIDs
			if sceneID != "" {
				var replacement *StyleScene
				for i := range l.Scenes {
					if l.Scenes[i].ID == sceneID {
						replacement = &l.Scenes[i]
					}
				}
				if replacement == nil {
					err = fmt.Errorf("include the requested scene")
				} else {
					next := base
					next.Scenes = append([]StyleScene(nil), base.Scenes...)
					for i := range next.Scenes {
						if next.Scenes[i].ID == sceneID {
							next.Scenes[i] = *replacement
						}
					}
					l = next
				}
			}
			if err == nil {
				l, err = s.Draft(ctx, l)
				if err == nil {
					return l, nil
				}
			}
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: raw}, llm.Message{Role: "user", Content: "Repair the complete proposal: " + err.Error()})
	}
	return Look{}, fmt.Errorf("model returned an invalid style: %w", err)
}
func (s *Service) Template(ctx context.Context, prompt string, base Look) (Look, error) {
	return s.Generate(ctx, prompt, "", base)
}

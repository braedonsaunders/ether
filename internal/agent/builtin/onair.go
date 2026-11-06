package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/onair"
	"strconv"
)

type OnAirService interface {
	Generate(context.Context, string, string, onair.Look) (onair.Look, error)
	GenerateScene(context.Context, string, string, onair.Look, string) (onair.Look, error)
	Draft(context.Context, onair.Look) (onair.Look, error)
	Look(context.Context, string) (onair.Look, error)
	SaveLook(context.Context, onair.Look) (onair.Look, error)
}
type channelLookWriter interface {
	Channel(context.Context, string) (livetv.Channel, error)
	SaveChannel(context.Context, livetv.Channel) (livetv.Channel, error)
}

func lookInput(in map[string]any) (onair.Look, error) {
	var l onair.Look
	b, e := json.Marshal(in["look"])
	if e != nil || len(b) > 64<<10 {
		return l, tools.Userf("look must be an object under 64 KB")
	}
	e = json.Unmarshal(b, &l)
	if e != nil {
		return l, tools.Userf("invalid look: %v", e)
	}
	return l, nil
}
func channelSummary(ctx context.Context, d Deps, id string) (map[string]any, error) {
	w, ok := d.LiveTV.(channelLookWriter)
	if !ok {
		return nil, tools.Userf("Live TV is unavailable")
	}
	c, e := w.Channel(ctx, id)
	if e != nil {
		return nil, e
	}
	titles := []string{}
	genres := map[string]int{}
	for _, it := range c.Items[:min(32, len(c.Items))] {
		if len(titles) < 8 {
			titles = append(titles, it.Title)
		}
		for _, g := range it.Genres {
			genres[g]++
		}
	}
	return map[string]any{"id": c.ID, "name": c.Name, "kind": c.Kind, "genres": genres, "titles": titles, "look": c.Look}, nil
}
func onairTools(d Deps) []tools.Tool {
	draft := tool(tools.Tool{Name: "look_draft", Title: "Draft a style", ReadOnly: true, OpenWorld: true, Description: "Draft a channel look from the owner's prompt and real channel summary using the configured model. Returns complete validated owned scenes, controls, motion and sound without saving. Include the current look as look to preserve unsaved edits, or lookId for a saved base. The owner saves with Save style.", InputSchema: schema(map[string]any{"prompt": pString("The owner's requested colours, type, mood and motion", 2000), "channelId": pString("Optional local channel id to match programming", 128), "lookId": pString("Optional saved base look id", 128), "sceneId": pString("Optional scene id to revise; preserves all other scenes", 128), "look": map[string]any{"type": "object", "additionalProperties": true, "description": "Optional complete current draft look"}}, "prompt"), Execute: func(ctx context.Context, _ tools.Principal, in map[string]any) (tools.Result, error) {
		if d.OnAir == nil {
			return tools.Result{}, tools.Userf("On Air is unavailable")
		}
		prompt, e := reqStr(in, "prompt", 2000)
		if e != nil {
			return tools.Result{}, e
		}
		id, e := optStr(in, "lookId", 128)
		if e != nil {
			return tools.Result{}, e
		}
		var base onair.Look
		if id != "" {
			base, e = d.OnAir.Look(ctx, id)
			if e != nil {
				return tools.Result{}, e
			}
		}
		if in["look"] != nil {
			base, e = lookInput(in)
			if e != nil {
				return tools.Result{}, e
			}
		}
		summary := ""
		channel, e := optStr(in, "channelId", 128)
		if e != nil {
			return tools.Result{}, e
		}
		if channel != "" {
			v, e := channelSummary(ctx, d, channel)
			if e != nil {
				return tools.Result{}, e
			}
			b, _ := json.Marshal(v)
			summary = string(b)
		}
		sceneID, e := optStr(in, "sceneId", 128)
		if e != nil {
			return tools.Result{}, e
		}
		l, e := d.OnAir.GenerateScene(ctx, prompt, summary, base, sceneID)
		if e != nil {
			return tools.Result{}, tools.Userf("%v", e)
		}
		return tools.Result{Data: map[string]any{"look": l}}, nil
	}})
	alias := draft
	alias.Name = "livetv_look_draft"
	// Short names are the console route's internal vocabulary; MCP exposes the
	// public namespaced draft tool rather than a duplicate or helper summary.
	consoleOnly := func(_ context.Context, p tools.Principal) bool { return p.Surface != "mcp" }
	draft.Visible = consoleOnly
	return []tools.Tool{draft, alias, tool(tools.Tool{Name: "channel_summary", Visible: consoleOnly, Title: "Channel programming summary", ReadOnly: true, Description: "Read a bounded local summary of a channel's real programming and current look, without contacting providers.", InputSchema: schema(map[string]any{"channelId": pString("The channel id", 128)}, "channelId"), Execute: func(ctx context.Context, _ tools.Principal, in map[string]any) (tools.Result, error) {
		id, e := reqStr(in, "channelId", 128)
		if e != nil {
			return tools.Result{}, e
		}
		v, e := channelSummary(ctx, d, id)
		return tools.Result{Data: v}, e
	}}),
		tool(tools.Tool{Name: "livetv_look_save", Title: "Save a channel look", Description: "Propose saving a validated On Air look. Nothing is written until the owner approves.", InputSchema: schema(map[string]any{"look": map[string]any{"type": "object", "additionalProperties": true}}, "look"), Execute: func(ctx context.Context, _ tools.Principal, in map[string]any) (tools.Result, error) {
			if d.OnAir == nil {
				return tools.Result{}, tools.Userf("On Air is unavailable")
			}
			l, e := lookInput(in)
			if e != nil {
				return tools.Result{}, e
			}
			l, e = d.OnAir.Draft(ctx, l)
			if e != nil {
				return tools.Result{}, tools.Userf("%v", e)
			}
			b, _ := json.Marshal(l)
			var v map[string]any
			json.Unmarshal(b, &v)
			return propose("Save a channel look", fmt.Sprintf("Save the look %q", l.Name), []tools.ProposalDetail{{Label: "Name", Value: l.Name}, {Label: "Template", Value: l.Template}, {Label: "Scenes", Value: strconv.Itoa(len(l.Scenes))}}, map[string]any{"look": v}, func(ctx context.Context) (tools.Result, error) {
				saved, e := d.OnAir.SaveLook(ctx, l)
				return tools.Result{Data: map[string]any{"look": saved}}, e
			}), nil
		}}),
		tool(tools.Tool{Name: "livetv_channel_look", Title: "Apply a channel look", Description: "Propose changing one channel's look and enabled moments. Preserves programming, audience and other channel settings. Nothing changes until owner approval.", InputSchema: schema(map[string]any{"channelId": pString("Local channel id", 128), "look": map[string]any{"type": "object", "additionalProperties": true, "description": "ChannelLook: lookId, ident, upNext, offAir and optional break, placement and overlay settings"}}, "channelId", "look"), Execute: func(ctx context.Context, _ tools.Principal, in map[string]any) (tools.Result, error) {
			w, ok := d.LiveTV.(channelLookWriter)
			if !ok || d.OnAir == nil {
				return tools.Result{}, tools.Userf("On Air is unavailable")
			}
			id, e := reqStr(in, "channelId", 128)
			if e != nil {
				return tools.Result{}, e
			}
			b, e := json.Marshal(in["look"])
			if e != nil {
				return tools.Result{}, e
			}
			var look livetv.ChannelLook
			if e = json.Unmarshal(b, &look); e != nil {
				return tools.Result{}, tools.Userf("invalid channel look")
			}
			if _, e = d.OnAir.Look(ctx, look.LookID); e != nil {
				return tools.Result{}, e
			}
			c, e := w.Channel(ctx, id)
			if e != nil {
				return tools.Result{}, e
			}
			return propose("Apply a channel look", fmt.Sprintf("Change the look on %q", c.Name), []tools.ProposalDetail{{Label: "Channel", Value: c.Name}, {Label: "Look", Value: look.LookID}}, in, func(ctx context.Context) (tools.Result, error) {
				current, e := w.Channel(ctx, id)
				if e != nil {
					return tools.Result{}, e
				}
				current.Look = &look
				saved, e := w.SaveChannel(ctx, current)
				return tools.Result{Data: map[string]any{"channel": saved.ID, "look": saved.Look}}, e
			}), nil
		}})}
}

package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/onair"
)

type channelCommercials interface {
	ImportChannelCommercials(context.Context, string, []string) (onair.Reel, error)
}

func validateChannelDraft(c livetv.Channel, links []string) error {
	if c.Kind != "virtual" || strings.TrimSpace(c.Name) == "" || len(c.Name) > 120 || strings.TrimSpace(c.Number) == "" || len(c.Number) > 32 {
		if strings.TrimSpace(c.Name) == "" || len(c.Name) > 120 {
			return tools.Userf("channel.name must contain 1–120 characters")
		}
		if strings.TrimSpace(c.Number) == "" || len(c.Number) > 32 {
			return tools.Userf("channel.number must contain 1–32 characters (a string, for example \"101\")")
		}
		return tools.Userf("channel.kind must be virtual")
	}
	if len(c.Items) > 10000 || len(c.Blocks) > 48 || len(c.Sources) > 32 || len(links) > 30 {
		return tools.Userf("channel proposal exceeds limits")
	}
	for i, b := range c.Blocks {
		for _, other := range c.Blocks[:i] {
			shared := len(b.Days) == 0 || len(other.Days) == 0
			for _, x := range b.Days {
				for _, y := range other.Days {
					shared = shared || x == y
				}
			}
			if shared && b.Start < other.End && other.Start < b.End {
				return tools.Userf("time blocks overlap")
			}
		}
		if b.Start < 0 || b.End > b.Start+1440 || b.End > 1440 || b.End <= b.Start || len(b.Items) > 10000 || len(b.Sources) > 32 {
			return tools.Userf("invalid time block")
		}
		for _, day := range b.Days {
			if day < 0 || day > 6 {
				return tools.Userf("invalid weekday")
			}
		}
	}
	for _, raw := range links {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
			return tools.Userf("commercials require HTTPS YouTube or Vimeo links")
		}
		h := strings.ToLower(u.Hostname())
		if h != "youtu.be" && h != "youtube.com" && h != "www.youtube.com" && h != "vimeo.com" && h != "www.vimeo.com" {
			return tools.Userf("commercials require YouTube or Vimeo links")
		}
	}
	return nil
}

func channelBuilderTools(d Deps) []tools.Tool {
	return []tools.Tool{tool(tools.Tool{Name: "livetv_channel_apply", OpenWorld: true, Title: "Apply channel", Description: `Propose creating or updating a virtual channel. Returns a reviewable before/after and never writes until owner approval. Submit channel programming fields (unrelated access and stream settings are preserved): id (empty for new), name, number, kind=virtual, enabled, items [{key,title,durationMs}], collection, filter, sources, blocks [{name,start,end,days,items,mode,collection,filter,sources}], mode (order,shuffle,random,marathon,newest), timezone, look. Catalogue and collection keys must be JSON numbers, not strings. Filters use {type:"movie"|"show",genres:["Comedy"],yearFrom:1980,yearTo:1989,minVotes:100,minRating:6}. Prefer bounded collection/filter sources over enumerating titles. Explicit item durations are milliseconds; fetch title_details for runtimeMinutes and multiply by 60000. No theme or branding: omit style and channel.look; do not call look_draft. Optional style is a complete validated On Air look from look_draft; it is saved only on Apply. commercialLinks holds at most 30 HTTPS YouTube/Vimeo video links, downloaded only on Apply, each limited to 120 seconds. Use web_search to find links from search terms first. Preserve all unrelated fields on updates.`, InputSchema: schema(map[string]any{"expectedRevision": pString("Opaque revision for approval replay; omit on first proposal", 64), "channel": map[string]any{"type": "object", "description": "Channel programming. For a new channel provide name and number. Optional fields may be omitted; no theme means omit look and style.", "additionalProperties": true, "properties": map[string]any{
		"id":   pString("Existing channel id; omit for a new channel", 128),
		"name": pString("Channel name", 120), "number": pString("Channel number as a string, e.g. 101", 32),
		"kind": pEnum("Channel kind", "virtual"), "enabled": pBool("Whether the channel is enabled"),
		"mode":       pEnum("Programming order", "order", "shuffle", "random", "marathon", "newest"),
		"collection": map[string]any{"type": "integer", "description": "Existing numeric collection key"},
		"filter":     map[string]any{"type": "object", "additionalProperties": true, "description": "Catalogue rules, e.g. {type:movie,genres:[Comedy],yearFrom:1980,yearTo:1989}"},
	}}, "style": map[string]any{"type": "object", "additionalProperties": true}, "commercialLinks": map[string]any{"type": "array", "maxItems": 30, "items": pString("YouTube or Vimeo video URL", 2048)}}, "channel"), Execute: func(ctx context.Context, _ tools.Principal, in map[string]any) (tools.Result, error) {
		w, ok := d.LiveTV.(channelLookWriter)
		if !ok {
			return tools.Result{}, tools.Userf("Live TV is unavailable")
		}
		b, e := json.Marshal(in)
		if e != nil || len(b) > 256<<10 {
			return tools.Result{}, tools.Userf("channel proposal too large")
		}
		var input struct {
			Channel livetv.Channel `json:"channel"`
			Style   *onair.Look    `json:"style"`
			Links   []string       `json:"commercialLinks"`
		}
		if e = json.Unmarshal(b, &input); e != nil {
			return tools.Result{}, tools.Userf("invalid channel proposal: %v", e)
		}
		c := input.Channel
		var before *livetv.Channel
		var original []byte
		if c.ID != "" {
			v, err := w.Channel(ctx, c.ID)
			if err != nil {
				return tools.Result{}, err
			}
			if v.Kind != "virtual" {
				return tools.Result{}, tools.Userf("only virtual channels can be programmed")
			}
			before = &v
			original, _ = json.Marshal(v)
			fingerprint := fmt.Sprintf("%x", sha256.Sum256(original))
			if expected, ok := in["expectedRevision"].(string); ok && expected != fingerprint {
				return tools.Result{}, tools.Userf("channel changed; review a fresh proposal")
			}
			in["expectedRevision"] = fingerprint
		}
		// Overlay only programming/branding fields; access, stream and owner
		// settings always come from the saved channel, never model output.
		base := livetv.Channel{Kind: "virtual", Enabled: true}
		if before != nil {
			base = *before
		}
		rawBase, _ := json.Marshal(base)
		merged := map[string]json.RawMessage{}
		_ = json.Unmarshal(rawBase, &merged)
		patchBytes, _ := json.Marshal(in["channel"])
		patch := map[string]json.RawMessage{}
		_ = json.Unmarshal(patchBytes, &patch)
		for _, key := range []string{"name", "number", "enabled", "items", "collection", "filter", "sources", "blocks", "mode", "align", "fit", "timezone", "shuffle", "look", "fallback", "offAir", "pins", "filler", "repeatDays", "watched", "seed", "epoch"} {
			if v, ok := patch[key]; ok {
				merged[key] = v
			}
		}
		mergedBytes, _ := json.Marshal(merged)
		_ = json.Unmarshal(mergedBytes, &c)
		if e = validateChannelDraft(c, input.Links); e != nil {
			return tools.Result{}, e
		}
		if checker, ok := d.LiveTV.(interface {
			DraftChannel(context.Context, livetv.Channel) (livetv.Channel, error)
		}); ok {
			if _, err := checker.DraftChannel(ctx, c); err != nil {
				return tools.Result{}, err
			}
		}
		if input.Style != nil {
			if d.OnAir == nil {
				return tools.Result{}, tools.Userf("On Air is unavailable")
			}
			l, err := d.OnAir.Draft(ctx, *input.Style)
			if err != nil {
				return tools.Result{}, err
			}
			input.Style = &l
			in["style"] = l
		}
		commercials, ok := d.OnAir.(channelCommercials)
		if len(input.Links) > 0 && !ok {
			return tools.Result{}, tools.Userf("commercial imports unavailable")
		}
		programming := func(items []livetv.Item, sources []livetv.ProgramSource, collection core.RatingKey, filter *core.Filter) string {
			names := []string{}
			for _, it := range items[:min(12, len(items))] {
				names = append(names, it.Title)
			}
			for _, src := range sources {
				names = append(names, src.Name)
			}
			if collection != 0 {
				names = append(names, fmt.Sprintf("Collection %d", collection))
			}
			if filter != nil {
				names = append(names, fmt.Sprintf("%s %s · %d–%d · rating %.1f+", filter.Type, strings.Join(filter.Genres, ", "), filter.YearFrom, filter.YearTo, filter.MinRating))
			}
			return fmt.Sprintf("%d titles; %s", len(items), strings.Join(names, ", "))
		}
		describe := func(v livetv.Channel) string {
			blocks := []string{}
			for _, b := range v.Blocks {
				blocks = append(blocks, fmt.Sprintf("%s %02d:%02d–%02d:%02d (%s)", b.Name, b.Start/60, b.Start%60, b.End/60, b.End%60, b.Mode)+" "+programming(b.Items, b.Sources, b.Collection, b.Filter))
			}
			return fmt.Sprintf("%s · %s · %s\n%s", v.Number, v.Name, v.Mode, programming(v.Items, v.Sources, v.Collection, v.Filter)+"\n"+strings.Join(blocks, "\n"))
		}
		old := "New channel"
		if before != nil {
			old = describe(*before)
		}
		details := []tools.ProposalDetail{{Label: "Before", Value: old}, {Label: "After", Value: describe(c)}, {Label: "Commercial clips", Value: fmt.Sprint(len(input.Links))}}
		breaks := "Off"
		if c.Look != nil && c.Look.Breaks != "" {
			breaks = c.Look.Breaks
		}
		if len(input.Links) > 0 {
			breaks = "Between programmes"
		}
		details = append(details, tools.ProposalDetail{Label: "Commercials", Value: breaks})
		if input.Style != nil {
			details = append(details, tools.ProposalDetail{Label: "Branding", Value: input.Style.Name})
		}
		for _, link := range input.Links {
			details = append(details, tools.ProposalDetail{Label: "Commercial", Value: link})
		}

		result := propose("Apply channel", fmt.Sprintf("Apply programming and branding for %q", c.Name), details, in, func(ctx context.Context) (tools.Result, error) {
			if before != nil {
				current, err := w.Channel(ctx, c.ID)
				if err != nil {
					return tools.Result{}, err
				}
				now, _ := json.Marshal(current)
				if string(now) != string(original) {
					return tools.Result{}, tools.Userf("channel changed; review a fresh proposal")
				}
			}
			if input.Style != nil {
				style := *input.Style
				style.ID = ""
				style.ChannelIDs = nil
				l, err := d.OnAir.SaveLook(ctx, style)
				if err != nil {
					return tools.Result{}, err
				}
				if c.Look == nil {
					c.Look = &livetv.ChannelLook{Ident: true, UpNext: true, OffAir: true}
				}
				c.Look.LookID = l.ID
			}
			if len(input.Links) > 0 {
				reel, err := commercials.ImportChannelCommercials(ctx, c.Name, input.Links)
				if err != nil {
					return tools.Result{}, err
				}
				if c.Look == nil {
					c.Look = &livetv.ChannelLook{}
				}
				c.Look.ReelID = reel.ID
				c.Look.BreakContent = "reel"
				c.Look.Breaks = "between"
				c.Look.BreakSeconds = 120
				c.Look.Placement = "insert"
			}
			saved, err := w.SaveChannel(ctx, c)
			return tools.Result{Data: map[string]any{"channel": saved}}, err
		})
		result.Data = map[string]any{"channel": c, "style": input.Style, "before": before, "status": "proposed"}
		return result, nil
	}})}
}

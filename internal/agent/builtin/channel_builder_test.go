package builtin

import (
	"context"
	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/livetv"
	"strings"
	"testing"
)

func TestChannelApplyValidation(t *testing.T) {
	valid := livetv.Channel{Name: "Cinema", Number: "10", Kind: "virtual", Blocks: []livetv.Block{{Start: 0, End: 720}, {Start: 720, End: 1440}}}
	if e := validateChannelDraft(valid, []string{"https://youtu.be/abc"}); e != nil {
		t.Fatal(e)
	}
	for _, links := range [][]string{{"http://youtube.com/watch?v=a"}, {"https://youtube.com.evil.test/a"}, {"https://user@youtube.com/a"}, strings.Fields(strings.Repeat("https://youtu.be/a ", 31))} {
		if validateChannelDraft(valid, links) == nil {
			t.Fatal("accepted unsafe or oversized links")
		}
	}
	valid.Blocks[1].Start = 700
	if validateChannelDraft(valid, nil) == nil {
		t.Fatal("accepted overlapping blocks")
	}
	valid.Blocks[1].Days = []int{8}
	if validateChannelDraft(valid, nil) == nil {
		t.Fatal("accepted invalid weekday")
	}
}

// Editor approvals must preserve owner edits and never publish a channel.
func TestChannelEditorDraftApproval(t *testing.T) {
	for _, id := range []string{"", "existing"} {
		t.Run(id, func(t *testing.T) {
			live := &editorChannelService{}
			tool := channelBuilderTools(Deps{LiveTV: live})[0]
			result, err := tool.Execute(context.Background(), tools.Principal{}, map[string]any{
				"editorChannel": map[string]any{"id": id, "kind": "virtual", "name": "Unsaved name", "number": "17", "enabled": true, "private": true, "timezone": "America/Toronto"},
				"channel":       map[string]any{"name": "Movie night", "filter": map[string]any{"type": "movie"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Proposal == nil {
				t.Fatal("missing approval")
			}
			applied, err := result.Proposal.Apply(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			channel := applied.Data["channel"].(livetv.Channel)
			if channel.ID != id || channel.Number != "17" || channel.Timezone != "America/Toronto" || channel.Name != "Movie night" || !channel.Private {
				t.Fatalf("lost editor fields: %+v", channel)
			}
			if live.reads != 0 || live.writes != 0 {
				t.Fatalf("editor touched saved channels: %+v", live)
			}
		})
	}
}

type editorChannelService struct{ reads, writes int }

func (*editorChannelService) Channels(context.Context) ([]livetv.Channel, error) { return nil, nil }
func (*editorChannelService) StreamSourceStatuses(context.Context, string) ([]livetv.StreamSourceStatus, error) {
	return nil, nil
}
func (*editorChannelService) ImportSource(context.Context, livetv.Source) (livetv.Source, error) {
	return livetv.Source{}, nil
}
func (s *editorChannelService) Channel(context.Context, string) (livetv.Channel, error) {
	s.reads++
	return livetv.Channel{}, nil
}
func (s *editorChannelService) SaveChannel(_ context.Context, c livetv.Channel) (livetv.Channel, error) {
	s.writes++
	return c, nil
}

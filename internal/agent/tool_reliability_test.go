package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/llm"
)

// The contract spans model requests, the bounded loop, durable transcript and
// approval SSE. Narration is never evidence that the owner has a card.
func TestPromisedToolCompletion(t *testing.T) {
	for _, comply := range []bool{true, false} {
		name := "never complies"
		if comply {
			name = "forced approval"
		}
		t.Run(name, func(t *testing.T) {
			narration := []llm.Event{{Kind: llm.EventTextDelta, Text: "Proceeding to create it now—please watch for the approval card and press Apply."}}
			scripts := [][]llm.Event{narration}
			if comply {
				scripts = append(scripts, callEv("proposal", "livetv_channel_apply", `{}`), []llm.Event{{Kind: llm.EventTextDelta, Text: "Ready to review."}})
			}
			model := &fakeModel{scripts: scripts}
			fx := newFixture(t, model, Options{})
			err := fx.svc.catalogue.Register(tools.Tool{
				Name: "livetv_channel_apply", Title: "Apply channel", Description: "Propose channel programming for approval", Owner: "builtin", Audience: tools.AudienceOwner,
				InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
				Execute: func(context.Context, tools.Principal, map[string]any) (tools.Result, error) {
					return tools.Result{Proposal: &tools.Proposal{Category: "Channel", Description: "Test", Apply: func(context.Context) (tools.Result, error) {
						t.Fatal("proposal executed without owner approval")
						return tools.Result{}, nil
					}}}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var chunks []Chunk
			thread, _, err := fx.svc.RunTurn(context.Background(), fx.p, "", "Create it, no theme", TurnContext{Route: "/livetv/channel-builder"}, func(c Chunk) error { chunks = append(chunks, c); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(model.requests) != 3 {
				t.Fatalf("expected bounded 3 requests, got %d", len(model.requests))
			}
			if model.requests[1].ToolChoice != "livetv_channel_apply" {
				t.Fatalf("call was not constrained: %+v", model.requests[1])
			}
			transcript, err := fx.store.AllMessages(context.Background(), thread)
			if err != nil {
				t.Fatal(err)
			}
			saved := string(transcript[len(transcript)-1].Parts)
			stream, _ := json.Marshal(chunks)
			if comply {
				if !strings.Contains(saved, `"type":"approval-request"`) || !strings.Contains(string(stream), `pending_approval`) {
					t.Fatalf("missing durable/streamed card: %s %s", saved, stream)
				}
			} else {
				if !strings.Contains(saved, missingToolNote) || !strings.Contains(string(stream), `"type":"error"`) {
					t.Fatalf("missing visible failure: %s %s", saved, stream)
				}
			}
		})
	}
}

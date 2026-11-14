package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// ErrToolsUnsupported reports that the configured provider or model cannot
// use tools. Callers show a clean "choose another model" message.
var ErrToolsUnsupported = errors.New("this model can't use tools")

// ToolDef is one tool offered to a model.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema object
}

// ToolCall is one model-requested tool call.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is one tool outcome fed back to the model.
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

// StreamMessage is a tool-capable chat message. Role is system, user,
// assistant or tool. Assistant messages may carry ToolCalls; tool messages
// carry ToolResults.
type StreamMessage struct {
	Role        string
	Content     string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// ToolRequest is one streaming tool-calling request.
type ToolRequest struct {
	System      string
	Messages    []StreamMessage
	Tools       []ToolDef
	Temperature float64
	MaxTokens   int
	// ToolChoice is empty for automatic selection, "required", or a tool name.
	ToolChoice string
}

// EventKind classifies stream events.
type EventKind string

const (
	EventTextDelta     EventKind = "text-delta"
	EventToolCallStart EventKind = "tool-call-start"
	EventToolCallDelta EventKind = "tool-call-delta"
	EventToolCall      EventKind = "tool-call"
	EventUsage         EventKind = "usage"
	EventDone          EventKind = "done"
	EventError         EventKind = "error"
)

// Event is one streamed model output unit.
type Event struct {
	Kind         EventKind
	Text         string
	ToolCallID   string
	ToolName     string
	InputDelta   string
	Input        json.RawMessage
	InputTokens  int
	OutputTokens int
	FinishReason string
	Err          error
}

// ProviderID returns the configured provider id.
func (c *Client) ProviderID() string {
	p, _ := c.config().Endpoint()
	return p.ID
}

// ModelName returns the configured model.
func (c *Client) ModelName() string { return c.config().Model }

// ToolsSupported reports whether tool calling can run, with a plain reason
// when it cannot.
func (c *Client) ToolsSupported() (bool, string) {
	cfg := c.config()
	if !cfg.Enabled {
		return false, "Language model is off."
	}
	p, base := cfg.Endpoint()
	if base == "" {
		return false, "No language model endpoint is configured."
	}
	if cfg.Model == "" {
		return false, "No model is chosen."
	}
	if !p.SupportsTools {
		return false, "This model can't use tools. Choose another model."
	}
	if !chatModel(cfg.Model) {
		return false, "This model can't use tools. Choose another model."
	}
	return true, ""
}

// toolMentionRe spots an error that is about tools or functions at all.
var toolMentionRe = regexp.MustCompile(`(?i)\b(tools?|functions?|function[-_ ]?calls?|tool_choice)\b`)

// toolUnsupportedRe spots the words providers use when a model or endpoint
// cannot take tools. Both this and toolMentionRe must match before a
// 400/404/422 becomes ErrToolsUnsupported; a generic 400 that merely names a
// tool (bad arguments, unknown tool name) keeps its own message.
var toolUnsupportedRe = regexp.MustCompile(`(?i)(not\s+(?:supported|available|allowed|permitted|enabled)|unsupport|does\s*n[o']?t\s+support|isn'?t\s+support|does\s*n[o']?t\s+(?:accept|allow|support)|is\s+not\s+supported|can'?t\s+(?:use|call|accept|handle)|no\s+(?:tools?|functions?)\b)`)

// isToolRefusal reports whether a provider error body clearly says tools or
// functions are unsupported by this model or endpoint.
func isToolRefusal(msg string) bool {
	return toolMentionRe.MatchString(msg) && toolUnsupportedRe.MatchString(msg)
}

func toolsRefusal(status int, body []byte) error {
	msg := providerMessage(body)
	if (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity) &&
		isToolRefusal(msg) {
		return fmt.Errorf("%w: %s", ErrToolsUnsupported, msg)
	}
	return fmt.Errorf("llm stream: %s", msg)
}

// StreamTools streams a tool-calling chat. The channel closes after a done
// or error event.
func (c *Client) StreamTools(ctx context.Context, req ToolRequest) (<-chan Event, error) {
	cfg := c.config()
	if ok, reason := c.ToolsSupported(); !ok {
		if !cfg.Enabled || cfg.Model == "" {
			return nil, fmt.Errorf("llm stream: %s", reason)
		}
		return nil, fmt.Errorf("%w: %s", ErrToolsUnsupported, reason)
	}
	p, base := cfg.Endpoint()
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		var err error
		finish := ""
		switch {
		// api.openai.com's reasoning models take function tools only on the
		// Responses API (/v1/responses); OpenAI-compatible servers (including
		// OpenRouter, Groq and custom endpoints) stay on chat/completions.
		case p.ID == "openai":
			finish, err = c.streamOpenAIResponses(ctx, base, cfg, req, out)
		case p.API == config.LLMAPIAnthropic:
			err = c.streamAnthropic(ctx, base, cfg, req, out)
		case p.API == config.LLMAPIGoogle:
			err = c.streamGemini(ctx, base, cfg, req, out)
		default:
			err = c.streamOpenAI(ctx, base, cfg, req, out)
		}
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			sendEvent(ctx, out, Event{Kind: EventError, Err: err})
			return
		}
		sendEvent(ctx, out, Event{Kind: EventDone, FinishReason: finish})
	}()
	return out, nil
}

func sendEvent(ctx context.Context, out chan<- Event, ev Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// StreamIdleTimeout bounds silence on a model stream. A stream that keeps
// sending runs until the caller's own deadline; the client's whole-request
// timeout would cut a long answer off mid-sentence.
const StreamIdleTimeout = 2 * time.Minute

// ErrStreamIdle reports a model stream that sent nothing for
// StreamIdleTimeout.
var ErrStreamIdle = errors.New("the language model sent nothing for 2 minutes")

// openStream POSTs and returns the SSE body, mapping tool refusals. The
// response is bounded by silence (StreamIdleTimeout) and ctx, not by the
// client's whole-request timeout.
func (c *Client) openStream(ctx context.Context, url string, h http.Header, body any) (io.ReadCloser, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	idle := time.AfterFunc(StreamIdleTimeout, func() { cancel(ErrStreamIdle) })
	stop := func() {
		idle.Stop()
		cancel(nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		stop()
		return nil, err
	}
	req.Header = h.Clone()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	streaming := *c.http
	streaming.Timeout = 0
	resp, err := streaming.Do(req)
	if err != nil {
		stop()
		if errors.Is(context.Cause(ctx), ErrStreamIdle) {
			return nil, ErrStreamIdle
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer stop()
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, toolsRefusal(resp.StatusCode, raw)
	}
	return &idleBody{ReadCloser: resp.Body, ctx: ctx, idle: idle, stop: stop}, nil
}

// idleBody re-arms the silence timer on every byte the model sends.
type idleBody struct {
	io.ReadCloser
	ctx  context.Context
	idle *time.Timer
	stop func()
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.idle.Reset(StreamIdleTimeout)
	}
	if err != nil && errors.Is(context.Cause(b.ctx), ErrStreamIdle) {
		err = ErrStreamIdle
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.stop()
	return b.ReadCloser.Close()
}

// scanSSE calls fn for each data: payload ([DONE] skipped).
func scanSSE(ctx context.Context, r io.Reader, fn func([]byte) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data strings.Builder
	flush := func() bool {
		if data.Len() == 0 {
			return true
		}
		s := data.String()
		data.Reset()
		if s == "[DONE]" {
			return true
		}
		return fn([]byte(s))
	}
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if !flush() {
				return nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

func schemaOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

// --- OpenAI and OpenAI-compatible ---

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaiMsg struct {
	Role       string `json:"role"`
	Content    string `json:"content,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
}

func (c *Client) streamOpenAI(ctx context.Context, base string, cfg config.LLM, req ToolRequest, out chan<- Event) error {
	msgs := make([]oaiMsg, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaiMsg{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			for _, r := range m.ToolResults {
				msgs = append(msgs, oaiMsg{Role: "tool", ToolCallID: r.ToolCallID, Content: r.Content})
			}
		case "assistant":
			om := oaiMsg{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				var call struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				call.ID, call.Type = tc.ID, "function"
				call.Function.Name = tc.Name
				call.Function.Arguments = string(schemaOrEmptyInput(tc.Input))
				om.ToolCalls = append(om.ToolCalls, call)
			}
			msgs = append(msgs, om)
		default:
			msgs = append(msgs, oaiMsg{Role: m.Role, Content: m.Content})
		}
	}
	tools := make([]oaiTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name, ot.Function.Description = t.Name, t.Description
		ot.Function.Parameters = schemaOrEmpty(t.InputSchema)
		tools = append(tools, ot)
	}
	body := map[string]any{
		"model": cfg.Model, "messages": msgs, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if req.ToolChoice != "" {
		choice := any(req.ToolChoice)
		if req.ToolChoice != "required" {
			choice = map[string]any{"type": "function", "function": map[string]any{"name": req.ToolChoice}}
		}
		body["tool_choice"] = choice
	}
	if req.Temperature != 0 {
		body["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	rc, err := c.openStream(ctx, base+"/chat/completions", openAIHeaders(cfg.APIKey), body)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "stream_options") {
		// Some OpenAI-compatible servers reject stream_options; retry without.
		delete(body, "stream_options")
		rc, err = c.openStream(ctx, base+"/chat/completions", openAIHeaders(cfg.APIKey), body)
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	type acc struct {
		id, name string
		args     strings.Builder
		started  bool
	}
	calls := map[int]*acc{}
	order := []int{}
	flushCalls := func() {
		for _, i := range order {
			a := calls[i]
			in := json.RawMessage(a.args.String())
			if !json.Valid(in) {
				in = json.RawMessage(`{}`)
			}
			sendEvent(ctx, out, Event{Kind: EventToolCall, ToolCallID: a.id, ToolName: a.name, Input: in})
		}
	}
	err = scanSSE(ctx, rc, func(data []byte) bool {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return true
		}
		if chunk.Error != nil {
			sendEvent(ctx, out, Event{Kind: EventError, Err: toolsRefusal(400, data)})
			return false
		}
		if chunk.Usage != nil {
			sendEvent(ctx, out, Event{Kind: EventUsage, InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens})
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				sendEvent(ctx, out, Event{Kind: EventTextDelta, Text: ch.Delta.Content})
			}
			for _, d := range ch.Delta.ToolCalls {
				a, ok := calls[d.Index]
				if !ok {
					a = &acc{}
					calls[d.Index] = a
					order = append(order, d.Index)
				}
				if d.ID != "" {
					a.id = d.ID
				}
				if d.Function.Name != "" {
					a.name = d.Function.Name
				}
				if !a.started && a.name != "" {
					a.started = true
					if a.id == "" {
						a.id = fmt.Sprintf("call_%d", d.Index)
					}
					sendEvent(ctx, out, Event{Kind: EventToolCallStart, ToolCallID: a.id, ToolName: a.name})
				}
				if d.Function.Arguments != "" {
					a.args.WriteString(d.Function.Arguments)
					sendEvent(ctx, out, Event{Kind: EventToolCallDelta, ToolCallID: a.id, ToolName: a.name, InputDelta: d.Function.Arguments})
				}
			}
		}
		return true
	})
	if err != nil {
		return err
	}
	flushCalls()
	return nil
}

func schemaOrEmptyInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// --- OpenAI Responses API (api.openai.com) ---
//
// OpenAI's newer reasoning models accept function tools (with reasoning)
// only on /v1/responses, and reject max_tokens and non-default
// temperatures there. Ether mirrors appkit-ai (Vercel AI SDK), whose OpenAI
// provider speaks the Responses API. It returns the finish reason the
// chat/completions path would report: "tool-calls" when a function call was
// emitted, else "stop".

type oaiResponsesInput struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type oaiResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict,omitempty"`
}

func (c *Client) streamOpenAIResponses(ctx context.Context, base string, cfg config.LLM, req ToolRequest, out chan<- Event) (string, error) {
	input := make([]oaiResponsesInput, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			for _, r := range m.ToolResults {
				input = append(input, oaiResponsesInput{Type: "function_call_output", CallID: r.ToolCallID, Output: r.Content})
			}
		case "assistant":
			if m.Content != "" {
				input = append(input, oaiResponsesInput{Type: "message", Role: "assistant", Content: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input = append(input, oaiResponsesInput{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: string(schemaOrEmptyInput(tc.Input))})
			}
		default:
			input = append(input, oaiResponsesInput{Type: "message", Role: m.Role, Content: m.Content})
		}
	}
	tools := make([]oaiResponsesTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, oaiResponsesTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: schemaOrEmpty(t.InputSchema)})
	}
	body := map[string]any{"model": cfg.Model, "input": input, "stream": true, "store": false}
	if req.System != "" {
		body["instructions"] = req.System
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if req.ToolChoice != "" {
		choice := any(req.ToolChoice)
		if req.ToolChoice != "required" {
			choice = map[string]any{"type": "function", "name": req.ToolChoice}
		}
		body["tool_choice"] = choice
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	rc, err := c.openStream(ctx, base+"/responses", openAIHeaders(cfg.APIKey), body)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	type acc struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*acc{}
	order := []int{}
	anyCall := false
	var failed error
	err = scanSSE(ctx, rc, func(data []byte) bool {
		var ev struct {
			Type   string `json:"type"`
			Delta  string `json:"delta"`
			ItemID string `json:"item_id"`
			Item   struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			OutputIndex int `json:"output_index"`
			Response    struct {
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return true
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				sendEvent(ctx, out, Event{Kind: EventTextDelta, Text: ev.Delta})
			}
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				id := ev.Item.CallID
				if id == "" {
					id = ev.Item.ID
				}
				a, ok := calls[ev.OutputIndex]
				if !ok {
					a = &acc{}
					calls[ev.OutputIndex] = a
					order = append(order, ev.OutputIndex)
				}
				a.id, a.name = id, ev.Item.Name
				anyCall = true
				sendEvent(ctx, out, Event{Kind: EventToolCallStart, ToolCallID: a.id, ToolName: a.name})
			}
		case "response.function_call_arguments.delta":
			a := calls[ev.OutputIndex]
			if a == nil {
				a = &acc{id: ev.ItemID}
				calls[ev.OutputIndex] = a
				order = append(order, ev.OutputIndex)
			}
			if ev.Delta != "" {
				a.args.WriteString(ev.Delta)
				sendEvent(ctx, out, Event{Kind: EventToolCallDelta, ToolCallID: a.id, ToolName: a.name, InputDelta: ev.Delta})
			}
		case "response.output_item.done":
			if ev.Item.Type == "function_call" {
				a := calls[ev.OutputIndex]
				if a == nil {
					a = &acc{}
					calls[ev.OutputIndex] = a
					order = append(order, ev.OutputIndex)
				}
				if a.id == "" {
					a.id = ev.Item.CallID
					if a.id == "" {
						a.id = ev.Item.ID
					}
				}
				if a.name == "" {
					a.name = ev.Item.Name
				}
				if a.args.Len() == 0 && ev.Item.Arguments != "" {
					a.args.WriteString(ev.Item.Arguments)
				}
				anyCall = true
			}
		case "response.completed":
			if u := ev.Response.Usage; u != nil && (u.InputTokens > 0 || u.OutputTokens > 0) {
				sendEvent(ctx, out, Event{Kind: EventUsage, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens})
			}
		case "response.failed", "error":
			msg := data
			if ev.Error != nil && ev.Error.Message != "" {
				msg = []byte(ev.Error.Message)
			} else if ev.Response.Error != nil && ev.Response.Error.Message != "" {
				msg = []byte(ev.Response.Error.Message)
			}
			// StreamTools reports the returned error as the one EventError.
			failed = toolsRefusal(400, msg)
			return false
		}
		return true
	})
	if err != nil {
		return "", err
	}
	if failed != nil {
		return "", failed
	}
	for _, i := range order {
		a := calls[i]
		in := json.RawMessage(a.args.String())
		if !json.Valid(in) {
			in = json.RawMessage(`{}`)
		}
		sendEvent(ctx, out, Event{Kind: EventToolCall, ToolCallID: a.id, ToolName: a.name, Input: in})
	}
	if anyCall {
		return "tool-calls", nil
	}
	return "stop", nil
}

// --- Anthropic Messages ---

func (c *Client) streamAnthropic(ctx context.Context, base string, cfg config.LLM, req ToolRequest, out chan<- Event) error {
	type block struct {
		Type      string          `json:"type"`
		Text      string          `json:"text,omitempty"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Input     json.RawMessage `json:"input,omitempty"`
		ToolUseID string          `json:"tool_use_id,omitempty"`
		Content   string          `json:"content,omitempty"`
		IsError   bool            `json:"is_error,omitempty"`
	}
	type msg struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}
	msgs := []msg{}
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			blocks := []block{}
			for _, r := range m.ToolResults {
				blocks = append(blocks, block{Type: "tool_result", ToolUseID: r.ToolCallID, Content: r.Content, IsError: r.IsError})
			}
			msgs = append(msgs, msg{Role: "user", Content: blocks})
		case "assistant":
			blocks := []block{}
			if m.Content != "" {
				blocks = append(blocks, block{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, block{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: schemaOrEmptyInput(tc.Input)})
			}
			msgs = append(msgs, msg{Role: "assistant", Content: blocks})
		case "system":
			// folded into top-level system by caller convention; append to req.System handling below is not possible, keep as user preamble is wrong: skip (agent passes System separately)
		default:
			msgs = append(msgs, msg{Role: "user", Content: []block{{Type: "text", Text: m.Content}}})
		}
	}
	tools := []map[string]any{}
	for _, t := range req.Tools {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": schemaOrEmpty(t.InputSchema)})
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = anthropicMaxTokens
	}
	body := map[string]any{"model": cfg.Model, "max_tokens": maxTokens, "stream": true, "messages": msgs}
	if req.System != "" {
		body["system"] = req.System
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if req.ToolChoice != "" {
		choice := map[string]any{"type": "any"}
		if req.ToolChoice != "required" {
			choice = map[string]any{"type": "tool", "name": req.ToolChoice}
		}
		body["tool_choice"] = choice
	}
	rc, err := c.openStream(ctx, base+"/messages", anthropicHeaders(cfg.APIKey), body)
	if err != nil {
		return err
	}
	defer rc.Close()
	type acc struct {
		id, name string
		args     strings.Builder
		tool     bool
	}
	blocks := map[int]*acc{}
	return scanSSE(ctx, rc, func(data []byte) bool {
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return true
		}
		switch ev.Type {
		case "error":
			if ev.Error != nil {
				sendEvent(ctx, out, Event{Kind: EventError, Err: toolsRefusal(400, data)})
				return false
			}
		case "message_start":
			if ev.Message.Usage.InputTokens > 0 {
				sendEvent(ctx, out, Event{Kind: EventUsage, InputTokens: ev.Message.Usage.InputTokens, OutputTokens: ev.Message.Usage.OutputTokens})
			}
		case "content_block_start":
			a := &acc{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name, tool: ev.ContentBlock.Type == "tool_use"}
			blocks[ev.Index] = a
			if a.tool {
				sendEvent(ctx, out, Event{Kind: EventToolCallStart, ToolCallID: a.id, ToolName: a.name})
			}
		case "content_block_delta":
			a := blocks[ev.Index]
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				sendEvent(ctx, out, Event{Kind: EventTextDelta, Text: ev.Delta.Text})
			}
			if ev.Delta.Type == "input_json_delta" && a != nil {
				a.args.WriteString(ev.Delta.PartialJSON)
				sendEvent(ctx, out, Event{Kind: EventToolCallDelta, ToolCallID: a.id, ToolName: a.name, InputDelta: ev.Delta.PartialJSON})
			}
		case "content_block_stop":
			if a := blocks[ev.Index]; a != nil && a.tool {
				in := json.RawMessage(a.args.String())
				if !json.Valid(in) {
					in = json.RawMessage(`{}`)
				}
				sendEvent(ctx, out, Event{Kind: EventToolCall, ToolCallID: a.id, ToolName: a.name, Input: in})
			}
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				sendEvent(ctx, out, Event{Kind: EventUsage, OutputTokens: ev.Usage.OutputTokens})
			}
		}
		return true
	})
}

// --- Google Gemini (native) ---

func geminiSchema(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(schemaOrEmpty(raw), &v) != nil {
		return schemaOrEmpty(raw)
	}
	var strip func(x any) any
	strip = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, val := range t {
				if k == "$schema" || k == "additionalProperties" {
					continue
				}
				out[k] = strip(val)
			}
			return out
		case []any:
			for i := range t {
				t[i] = strip(t[i])
			}
			return t
		default:
			return x
		}
	}
	b, _ := json.Marshal(strip(v))
	return b
}

func (c *Client) streamGemini(ctx context.Context, base string, cfg config.LLM, req ToolRequest, out chan<- Event) error {
	type part struct {
		Text             string `json:"text,omitempty"`
		FunctionCall     any    `json:"functionCall,omitempty"`
		FunctionResponse any    `json:"functionResponse,omitempty"`
	}
	type content struct {
		Role  string `json:"role"`
		Parts []part `json:"parts"`
	}
	contents := []content{}
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			parts := []part{}
			for _, r := range m.ToolResults {
				var parsed any
				if json.Unmarshal([]byte(r.Content), &parsed) != nil {
					parsed = map[string]any{"content": r.Content}
				} else if _, isObj := parsed.(map[string]any); !isObj {
					// Gemini functionResponse.response must be an object.
					parsed = map[string]any{"content": parsed}
				}
				parts = append(parts, part{FunctionResponse: map[string]any{"name": r.Name, "response": parsed}})
			}
			contents = append(contents, content{Role: "user", Parts: parts})
		case "assistant":
			parts := []part{}
			if m.Content != "" {
				parts = append(parts, part{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var args any
				_ = json.Unmarshal(schemaOrEmptyInput(tc.Input), &args)
				parts = append(parts, part{FunctionCall: map[string]any{"name": tc.Name, "args": args}})
			}
			contents = append(contents, content{Role: "model", Parts: parts})
		case "system":
		default:
			contents = append(contents, content{Role: "user", Parts: []part{{Text: m.Content}}})
		}
	}
	decls := []map[string]any{}
	for _, t := range req.Tools {
		decls = append(decls, map[string]any{"name": t.Name, "description": t.Description, "parameters": geminiSchema(t.InputSchema)})
	}
	body := map[string]any{"contents": contents}
	if req.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": req.System}}}
	}
	if len(decls) > 0 {
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	if req.ToolChoice != "" {
		choice := map[string]any{"mode": "ANY"}
		if req.ToolChoice != "required" {
			choice["allowedFunctionNames"] = []string{req.ToolChoice}
		}
		body["toolConfig"] = map[string]any{"functionCallingConfig": choice}
	}
	if req.Temperature != 0 {
		body["generationConfig"] = map[string]any{"temperature": req.Temperature}
	}
	h := http.Header{}
	h.Set("x-goog-api-key", cfg.APIKey)
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", base, cfg.Model)
	rc, err := c.openStream(ctx, url, h, body)
	if err != nil {
		return err
	}
	defer rc.Close()
	callN := 0
	return scanSSE(ctx, rc, func(data []byte) bool {
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text         string `json:"text"`
						FunctionCall *struct {
							Name string          `json:"name"`
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return true
		}
		if chunk.Error != nil {
			sendEvent(ctx, out, Event{Kind: EventError, Err: toolsRefusal(400, data)})
			return false
		}
		if chunk.UsageMetadata != nil {
			sendEvent(ctx, out, Event{Kind: EventUsage, InputTokens: chunk.UsageMetadata.PromptTokenCount, OutputTokens: chunk.UsageMetadata.CandidatesTokenCount})
		}
		for _, cand := range chunk.Candidates {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					sendEvent(ctx, out, Event{Kind: EventTextDelta, Text: p.Text})
				}
				if p.FunctionCall != nil {
					callN++
					id := fmt.Sprintf("call_%d", callN)
					args := p.FunctionCall.Args
					if len(args) == 0 {
						args = json.RawMessage(`{}`)
					}
					sendEvent(ctx, out, Event{Kind: EventToolCallStart, ToolCallID: id, ToolName: p.FunctionCall.Name})
					sendEvent(ctx, out, Event{Kind: EventToolCallDelta, ToolCallID: id, ToolName: p.FunctionCall.Name, InputDelta: string(args)})
					sendEvent(ctx, out, Event{Kind: EventToolCall, ToolCallID: id, ToolName: p.FunctionCall.Name, Input: args})
				}
			}
		}
		return true
	})
}

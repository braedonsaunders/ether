// Package llm is an optional multi-provider chat client (OpenAI, Anthropic,
// Gemini, OpenRouter, Ollama, LM Studio, any OpenAI-compatible server, …)
// used for natural-language recommendation rows:
// "more like this" for a title and themed rows from a user's history
// ("Slow-burn sci-fi with a twist"). The model only ever returns
// titles+years; they are resolved to catalogue items via core.Catalog, so
// hallucinated titles simply drop out. Everything is off unless
// config.LLM.Enabled, results are cached, and Home rows are served
// stale-or-omitted while a refresh runs in the background — the LLM never
// blocks a Home request.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/egress"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client talks to the owner's chosen provider (config.LLMProviders).
type Client struct {
	cfg  config.LLM
	http *http.Client
	log  *slog.Logger
	mu   sync.RWMutex
}

// ClientOptions build a Client from a config snapshot.
type ClientOptions struct {
	Config     config.LLM
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// NewClient builds a Client.
func NewClient(opts ClientOptions) *Client {
	if opts.HTTPClient == nil {
		opts.HTTPClient = egress.Client(egress.CapMetadata, 120*time.Second)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{cfg: opts.Config, http: opts.HTTPClient, log: opts.Logger}
}

// UpdateConfig swaps the LLM settings.
func (c *Client) UpdateConfig(cfg config.LLM) {
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
}

// Enabled reports whether the LLM is configured on.
func (c *Client) Enabled() bool {
	cfg := c.config()
	_, base := cfg.Endpoint()
	return cfg.Enabled && base != "" && cfg.Model != ""
}

func (c *Client) config() config.LLM {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []Message      `json:"messages"`
	Temperature    *float64       `json:"temperature,omitempty"`
	ResponseFormat map[string]any `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// temperature keeps suggestions varied without drifting off the request.
const temperature = 0.4

// Chat sends messages and returns the assistant content. Anthropic is
// spoken natively; Gemini and every other provider through the OpenAI
// chat-completions shape.
func (c *Client) Chat(ctx context.Context, msgs []Message, jsonMode bool) (string, error) {
	cfg := c.config()
	if !cfg.Enabled {
		return "", fmt.Errorf("llm disabled")
	}
	p, base := cfg.Endpoint()
	if base == "" || cfg.Model == "" {
		return "", fmt.Errorf("llm endpoint and model are required")
	}
	var content string
	var err error
	switch p.API {
	case config.LLMAPIAnthropic:
		content, err = c.anthropicChat(ctx, base, cfg, msgs)
	case config.LLMAPIGoogle:
		content, err = c.openAIChat(ctx, base+"/openai", cfg, msgs, jsonMode)
	default:
		content, err = c.openAIChat(ctx, base, cfg, msgs, jsonMode)
	}
	if err != nil {
		return "", err
	}
	return StripReasoning(content), nil
}

// openAIChat first asks for response_format=json_object (when jsonMode is
// set) and a low temperature, and drops either on a 400/422: many local
// servers reject response_format, and reasoning models (o-series, GPT-5)
// reject any temperature but the default. The prompt still asks for JSON
// and the parsers below are tolerant.
func (c *Client) openAIChat(ctx context.Context, base string, cfg config.LLM, msgs []Message, jsonMode bool) (string, error) {
	temp := true
	for {
		content, status, err := c.chatOnce(ctx, base, cfg, msgs, jsonMode, temp)
		if err == nil || (status != http.StatusBadRequest && status != http.StatusUnprocessableEntity) {
			return content, err
		}
		switch {
		case temp && strings.Contains(strings.ToLower(err.Error()), "temperature"):
			temp = false
		case jsonMode:
			jsonMode = false
		case temp:
			temp = false
		default:
			return "", err
		}
		c.log.Debug("llm rejected a request option, retrying without it", "err", err)
	}
}

var thinkRe = regexp.MustCompile(`(?is)<(think|thinking|reasoning)>.*?</(think|thinking|reasoning)>`)

// StripReasoning removes <think>…</think>-style blocks that reasoning models
// (DeepSeek-R1, Qwen3, …) emit before their answer, plus an unterminated
// leading block when the model ran out of tokens mid-thought.
func StripReasoning(content string) string {
	content = thinkRe.ReplaceAllString(content, "")
	low := strings.ToLower(content)
	for _, tag := range []string{"<think>", "<thinking>", "<reasoning>"} {
		if i := strings.Index(low, tag); i >= 0 {
			content = content[:i]
			break
		}
	}
	return strings.TrimSpace(content)
}

func (c *Client) chatOnce(ctx context.Context, base string, cfg config.LLM, msgs []Message, jsonMode, temp bool) (string, int, error) {
	cr := chatRequest{Model: cfg.Model, Messages: msgs}
	if temp {
		t := temperature
		cr.Temperature = &t
	}
	if jsonMode {
		cr.ResponseFormat = map[string]any{"type": "json_object"}
	}
	var out chatResponse
	status, err := c.postJSON(ctx, base+"/chat/completions", openAIHeaders(cfg.APIKey), cr, &out)
	if err != nil {
		return "", status, fmt.Errorf("llm chat: %w", err)
	}
	if out.Error != nil {
		return "", status, fmt.Errorf("llm chat: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", status, fmt.Errorf("llm chat: no choices")
	}
	return out.Choices[0].Message.Content, status, nil
}

func openAIHeaders(key string) http.Header {
	h := http.Header{}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	return h
}

// postJSON POSTs body and decodes a 200 response into out. Non-200
// responses become errors carrying the provider's message.
func (c *Client) postJSON(ctx context.Context, url string, h http.Header, body, out any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header = h.Clone()
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) (int, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, providerMessage(b))
	}
	if err := json.Unmarshal(b, out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode: %w", err)
	}
	return resp.StatusCode, nil
}

// providerMessage pulls error.message out of an error body (OpenAI and
// Anthropic both use that shape; Gemini wraps it in a one-element array),
// falling back to the raw text.
func providerMessage(b []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return truncate(e.Error.Message, 300)
	}
	var arr []struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &arr) == nil && len(arr) > 0 && arr[0].Error.Message != "" {
		return truncate(arr[0].Error.Message, 300)
	}
	return truncate(strings.TrimSpace(string(b)), 200)
}

// TitleRef is a title the model suggested.
type TitleRef struct {
	Title  string `json:"title"`
	Year   int    `json:"year,omitempty"`
	Type   string `json:"type,omitempty"` // movie|show|tv
	Reason string `json:"reason,omitempty"`
}

// ThemedRow is one named row of suggestions.
type ThemedRow struct {
	Title  string     `json:"title"`
	Reason string     `json:"reason,omitempty"`
	Titles []TitleRef `json:"titles"`
}

// flexible decoding: models variously emit {"titles":[...]}, {"rows":[...]},
// a bare array, year as string, title under "name", etc.
type flexRef struct {
	Title  string          `json:"title"`
	Name   string          `json:"name"`
	Year   json.RawMessage `json:"year"`
	Type   string          `json:"type"`
	Reason string          `json:"reason"`
	Why    string          `json:"why"`
}

func (f flexRef) ref() TitleRef {
	r := TitleRef{Title: f.Title, Type: f.Type, Reason: f.Reason}
	if r.Title == "" {
		r.Title = f.Name
	}
	if r.Reason == "" {
		r.Reason = f.Why
	}
	if len(f.Year) > 0 {
		var n int
		if err := json.Unmarshal(f.Year, &n); err == nil {
			r.Year = n
		} else {
			var s string
			if err := json.Unmarshal(f.Year, &s); err == nil {
				r.Year, _ = strconv.Atoi(strings.TrimSpace(s))
			}
		}
	}
	r.Title = strings.TrimSpace(r.Title)
	return r
}

type flexRow struct {
	Title  string          `json:"title"`
	Name   string          `json:"name"`
	Theme  string          `json:"theme"`
	Reason string          `json:"reason"`
	Titles json.RawMessage `json:"titles"`
	Items  json.RawMessage `json:"items"`
	Movies json.RawMessage `json:"movies"`
}

func parseRefs(raw json.RawMessage) []TitleRef {
	var flex []flexRef
	if err := json.Unmarshal(raw, &flex); err == nil {
		out := make([]TitleRef, 0, len(flex))
		for _, f := range flex {
			if r := f.ref(); r.Title != "" {
				out = append(out, r)
			}
		}
		return out
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		out := make([]TitleRef, 0, len(strs))
		for _, s := range strs {
			out = append(out, parseLineRef(s))
		}
		return out
	}
	return nil
}

var lineRefRe = regexp.MustCompile(`^(?:\d+[.)]\s*|[-*]\s*)?(.+?)(?:\s*\((\d{4})\))?\s*$`)

func parseLineRef(line string) TitleRef {
	line = strings.Trim(line, " \t\"'")
	m := lineRefRe.FindStringSubmatch(line)
	if m == nil {
		return TitleRef{Title: line}
	}
	r := TitleRef{Title: strings.Trim(m[1], " \t\"'")}
	if m[2] != "" {
		r.Year, _ = strconv.Atoi(m[2])
	}
	return r
}

var trailingCommaRe = regexp.MustCompile(`,(\s*[}\]])`)

// repairJSON fixes the two most common model JSON faults: trailing commas
// and output truncated by a token limit (closed after the last complete
// object/array, dropping the partial tail). Valid input is returned as is.
func repairJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || json.Valid([]byte(s)) {
		return s
	}
	s = trailingCommaRe.ReplaceAllString(s, "$1")
	if json.Valid([]byte(s)) {
		return s
	}
	var stack, safeStack []byte
	inStr, esc := false, false
	lastSafe := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			lastSafe = i + 1
			safeStack = append(safeStack[:0], stack...)
		}
	}
	if lastSafe < 0 {
		return s
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(s[:lastSafe], " \t\r\n,"))
	for i := len(safeStack) - 1; i >= 0; i-- {
		if safeStack[i] == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
	return b.String()
}

// extractJSON finds the first balanced {...} or [...] in model output,
// tolerating code fences and prose around it, and repairs common faults.
func extractJSON(s string) string {
	return repairJSON(extractJSONRaw(s))
}

func extractJSONRaw(s string) string {
	s = strings.ReplaceAll(s, "```json", "```")
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if j := strings.Index(rest, "```"); j >= 0 {
			s = rest[:j]
		}
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return ""
	}
	open, closeCh := s[start], byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == open:
			depth++
		case c == closeCh:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}

// ParseTitles extracts title refs from model output in whatever shape it
// arrived: {"titles":[...]}, {"rows":[...]}, a bare array, or a plain list
// of "Title (Year)" lines.
func ParseTitles(content string) []TitleRef {
	if rows := ParseRows(content); len(rows) > 0 {
		var out []TitleRef
		for _, r := range rows {
			out = append(out, r.Titles...)
		}
		if len(out) > 0 {
			return out
		}
	}
	js := extractJSON(content)
	if js != "" {
		if strings.HasPrefix(js, "[") {
			if refs := parseRefs(json.RawMessage(js)); len(refs) > 0 {
				return refs
			}
		} else {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(js), &obj); err == nil {
				for _, key := range []string{"titles", "items", "movies", "shows", "recommendations", "results"} {
					if raw, ok := obj[key]; ok {
						if refs := parseRefs(raw); len(refs) > 0 {
							return refs
						}
					}
				}
			}
		}
	}
	// Line-by-line fallback.
	var out []TitleRef
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "{") || strings.HasPrefix(line, "}") || strings.Contains(line, "\"") && strings.Contains(line, ":") {
			continue
		}
		if r := parseLineRef(strings.TrimSuffix(line, ",")); r.Title != "" && len(r.Title) < 120 {
			out = append(out, r)
		}
	}
	return out
}

// ParseRows extracts themed rows from model output.
func ParseRows(content string) []ThemedRow {
	js := extractJSON(content)
	if js == "" {
		if refs := ParseTitlesLinesOnly(content); len(refs) > 0 {
			return []ThemedRow{{Title: "Picked for you", Titles: refs}}
		}
		return nil
	}
	parseFlexRows := func(raw json.RawMessage) []ThemedRow {
		var flex []flexRow
		if err := json.Unmarshal(raw, &flex); err != nil {
			return nil
		}
		out := make([]ThemedRow, 0, len(flex))
		for _, f := range flex {
			title := f.Title
			if title == "" {
				title = f.Name
			}
			if title == "" {
				title = f.Theme
			}
			raw := f.Titles
			if len(raw) == 0 {
				raw = f.Items
			}
			if len(raw) == 0 {
				raw = f.Movies
			}
			refs := parseRefs(raw)
			if len(refs) > 0 {
				out = append(out, ThemedRow{Title: title, Reason: f.Reason, Titles: refs})
			}
		}
		return out
	}
	if strings.HasPrefix(js, "[") {
		if rows := parseFlexRows(json.RawMessage(js)); len(rows) > 0 {
			return rows
		}
		if refs := parseRefs(json.RawMessage(js)); len(refs) > 0 {
			return []ThemedRow{{Title: "Picked for you", Titles: refs}}
		}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(js), &obj); err != nil {
		return nil
	}
	for _, key := range []string{"rows", "themes", "collections", "hubs"} {
		if raw, ok := obj[key]; ok {
			if rows := parseFlexRows(raw); len(rows) > 0 {
				return rows
			}
		}
	}
	// A single named row object: {"title":"theme","titles":[...]}.
	_, hasTitle := obj["title"]
	_, hasName := obj["name"]
	_, hasTheme := obj["theme"]
	if hasTitle || hasName || hasTheme {
		if rows := parseFlexRows(json.RawMessage("[" + js + "]")); len(rows) > 0 && rows[0].Title != "" {
			return rows
		}
	}
	for _, key := range []string{"titles", "items", "movies", "shows", "recommendations", "results"} {
		if raw, ok := obj[key]; ok {
			if refs := parseRefs(raw); len(refs) > 0 {
				return []ThemedRow{{Title: "Picked for you", Titles: refs}}
			}
		}
	}
	return nil
}

// ParseTitlesLinesOnly is the plain-text fallback used by ParseRows.
func ParseTitlesLinesOnly(content string) []TitleRef {
	if extractJSON(content) != "" {
		return nil
	}
	var out []TitleRef
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := parseLineRef(line); r.Title != "" && len(r.Title) < 120 {
			out = append(out, r)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

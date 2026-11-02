// Package tools is Ether's one tool catalogue. The Ask Ether agent and the
// MCP server both read it; plugin tools join it through the plugin host.
//
// It mirrors AppKit's McpCatalogTool contract (appkit-mcp): a hidden tool is
// never offered to a model, every tool ends in an existing gated service and
// never a parallel query path, inputs are re-validated in Execute, every call
// is audited, and only mapped error messages reach a model.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// Audience says who may use a tool.
type Audience string

const (
	AudienceOwner  Audience = "owner"
	AudienceViewer Audience = "viewer" // later phase; no viewer tools in v1
)

// Principal is the authenticated actor a tool runs as.
type Principal struct {
	ViewerID int64
	IsOwner  bool
	// Surface is "console" for Ask Ether, "mcp" for paired MCP clients.
	Surface string
	// Scopes holds grant scopes ("read", "full") for MCP; empty for console.
	Scopes []string
	// Plugin is set when a plugin acts through the catalogue.
	Plugin string
}

// HasScope reports whether p carries scope s. Console principals carry all.
func (p Principal) HasScope(s string) bool {
	if p.Surface == "console" {
		return true
	}
	for _, have := range p.Scopes {
		if have == s || have == "full" {
			return true
		}
	}
	return false
}

// Link is a console deep link a result may offer.
type Link struct {
	Label string `json:"label"` // English source; the console translates it
	Route string `json:"route"` // console route, e.g. "/releases/history/42"
}

// Proposal is a write the owner must approve before it runs. Tools that
// change anything return a Proposal instead of acting.
type Proposal struct {
	// Category is a short English label for the approval card ("Blocklist a release").
	Category    string           `json:"category"`
	Description string           `json:"description"`
	Details     []ProposalDetail `json:"details,omitempty"`
	// Apply runs the change after approval. It must be idempotent.
	Apply func(ctx context.Context) (Result, error) `json:"-"`
	// Input is the validated input, stored with the approval for audit.
	Input map[string]any `json:"input,omitempty"`
}

// ProposalDetail is one labelled value shown on an approval card.
type ProposalDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Result is what a tool returns.
type Result struct {
	// Data is sent to the model. Keep it compact and bounded.
	Data map[string]any `json:"data,omitempty"`
	// Links are offered in the console next to the answer.
	Links []Link `json:"links,omitempty"`
	// Navigate asks the console to open a route (the navigate tool).
	Navigate string `json:"navigate,omitempty"`
	// Proposal, when set, becomes an approval card; nothing has changed yet.
	Proposal *Proposal `json:"proposal,omitempty"`
}

// Tool is one catalogue entry.
type Tool struct {
	Name        string          // snake_case; plugin tools are "<plugin>__<name>" with dots as _
	Title       string          // English source, translated in the console
	Description string          // for the model
	InputSchema json.RawMessage // JSON Schema (object); Execute re-validates
	ReadOnly    bool
	Destructive bool
	// OpenWorld marks tools that reach external services or return text that
	// third parties control (release names, TMDB overviews, plugin output).
	OpenWorld bool
	Audience  Audience
	// Owner is "builtin" or a plugin id.
	Owner string
	// Visible hides a tool from a principal entirely. Nil means visible.
	Visible   func(ctx context.Context, p Principal) bool
	Execute   func(ctx context.Context, p Principal, input map[string]any) (Result, error)
	Summarize func(Result) string
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

// Validate checks a tool's static contract.
func (t Tool) Validate() error {
	switch {
	case !namePattern.MatchString(t.Name):
		return fmt.Errorf("tool name %q must be snake_case", t.Name)
	case t.Title == "" || t.Description == "":
		return fmt.Errorf("tool %s: title and description are required", t.Name)
	case t.Execute == nil:
		return fmt.Errorf("tool %s: execute is required", t.Name)
	case t.ReadOnly && t.Destructive:
		return fmt.Errorf("tool %s: cannot be both read-only and destructive", t.Name)
	case t.Audience != AudienceOwner && t.Audience != AudienceViewer:
		return fmt.Errorf("tool %s: unknown audience %q", t.Name, t.Audience)
	case t.Owner == "":
		return fmt.Errorf("tool %s: owner is required", t.Name)
	}
	var schema map[string]any
	if len(t.InputSchema) == 0 || json.Unmarshal(t.InputSchema, &schema) != nil || schema["type"] != "object" {
		return fmt.Errorf("tool %s: input schema must be a JSON object schema", t.Name)
	}
	return nil
}

// UserError is an error whose message is safe to show a model and a person.
// Every other error reaches a model only as a generic failure.
type UserError struct{ Message string }

func (e *UserError) Error() string { return e.Message }

// Userf returns a UserError.
func Userf(format string, args ...any) error {
	return &UserError{Message: fmt.Sprintf(format, args...)}
}

// SafeMessage maps err to text a model may see.
func SafeMessage(err error) string {
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Message
	}
	return "The tool failed. Details are in the server log."
}

// Catalogue is the set of tools. It is safe for concurrent use.
type Catalogue struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewCatalogue returns an empty catalogue.
func NewCatalogue() *Catalogue { return &Catalogue{tools: map[string]Tool{}} }

// Register adds tools; it is all or nothing.
func (c *Catalogue) Register(ts ...Tool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	for _, t := range ts {
		if err := t.Validate(); err != nil {
			return err
		}
		if _, ok := c.tools[t.Name]; ok || seen[t.Name] {
			return fmt.Errorf("tool %s already registered", t.Name)
		}
		seen[t.Name] = true
	}
	for _, t := range ts {
		c.tools[t.Name] = t
	}
	return nil
}

// ReplaceOwner swaps every tool owned by a plugin.
func (c *Catalogue) ReplaceOwner(owner string, ts []Tool) error {
	if owner == "" || owner == "builtin" {
		return fmt.Errorf("cannot replace tools of owner %q", owner)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range ts {
		if t.Owner != owner {
			return fmt.Errorf("tool %s: owner %q does not match %q", t.Name, t.Owner, owner)
		}
		if err := t.Validate(); err != nil {
			return err
		}
		if existing, ok := c.tools[t.Name]; ok && existing.Owner != owner {
			return fmt.Errorf("tool %s already registered", t.Name)
		}
	}
	for name, t := range c.tools {
		if t.Owner == owner {
			delete(c.tools, name)
		}
	}
	for _, t := range ts {
		c.tools[t.Name] = t
	}
	return nil
}

// Visible returns the tools p may see, sorted by name. Read-only principals
// (MCP "read" scope) see only read-only tools.
func (c *Catalogue) Visible(ctx context.Context, p Principal) []Tool {
	c.mu.RLock()
	out := make([]Tool, 0, len(c.tools))
	for _, t := range c.tools {
		if t.Audience == AudienceOwner && !p.IsOwner {
			continue
		}
		if !t.ReadOnly && !p.HasScope("full") {
			continue
		}
		if t.Visible != nil && !t.Visible(ctx, p) {
			continue
		}
		out = append(out, t)
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns a tool by name only if p may see it.
func (c *Catalogue) Lookup(ctx context.Context, p Principal, name string) (Tool, bool) {
	for _, t := range c.Visible(ctx, p) {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

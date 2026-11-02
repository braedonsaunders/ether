package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func tool(name string, readOnly bool) Tool {
	return Tool{
		Name: name, Title: "T", Description: "D", ReadOnly: readOnly, Audience: AudienceOwner, Owner: "builtin",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:     func(context.Context, Principal, map[string]any) (Result, error) { return Result{}, nil },
	}
}

func TestCatalogueVisibility(t *testing.T) {
	c := NewCatalogue()
	if err := c.Register(tool("server_status", true), tool("server_restart", false)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	console := Principal{IsOwner: true, Surface: "console"}
	if got := c.Visible(ctx, console); len(got) != 2 {
		t.Fatalf("console sees %d tools", len(got))
	}
	readOnly := Principal{IsOwner: true, Surface: "mcp", Scopes: []string{"read"}}
	if got := c.Visible(ctx, readOnly); len(got) != 1 || got[0].Name != "server_status" {
		t.Fatalf("read scope sees %+v", got)
	}
	if _, ok := c.Lookup(ctx, readOnly, "server_restart"); ok {
		t.Fatal("read scope must not look up write tools")
	}
	if got := c.Visible(ctx, Principal{Surface: "console"}); len(got) != 0 {
		t.Fatal("non-owners see no owner tools")
	}
	if err := c.Register(tool("server_status", true)); err == nil {
		t.Fatal("duplicate accepted")
	}
	bad := tool("Bad Name", true)
	if err := c.Register(bad); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestReplaceOwnerAndSafeMessage(t *testing.T) {
	c := NewCatalogue()
	pt := tool("dev_example__lights", false)
	pt.Owner = "dev.example"
	if err := c.ReplaceOwner("dev.example", []Tool{pt}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReplaceOwner("dev.example", nil); err != nil {
		t.Fatal(err)
	}
	if got := c.Visible(context.Background(), Principal{IsOwner: true, Surface: "console"}); len(got) != 0 {
		t.Fatalf("left %+v", got)
	}
	if SafeMessage(Userf("no such title")) != "no such title" {
		t.Fatal("user errors pass through")
	}
	if SafeMessage(errors.New("pq: syntax error at SELECT")) == "pq: syntax error at SELECT" {
		t.Fatal("internal errors must not reach a model")
	}
}

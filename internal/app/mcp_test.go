package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/resolver"
)

func TestMCPBuiltinToolsAndEndToEndWorkflow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	a, err := New(ctx, Options{
		DataDir: dir,
		Version: "2.0.0-test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)

	// 1. Verify all 37 built-in tools are registered in both the tool catalogue
	// and the descriptor registry (extend.KindTool). web_search and web_fetch
	// are among them but stay invisible to MCP principals.
	toolDescs := a.extendRT.registry.List(extend.KindTool)
	if len(toolDescs) != 38 {
		t.Fatalf("expected 38 extend.KindTool descriptors, got %d", len(toolDescs))
	}

	// Route-to-tool coverage for the existing guide match endpoint. Both MCP
	// and Ask Ether must resolve to the same owner-gated service operation.
	guideProbe := httptest.NewRecorder()
	a.livetv.Handler().ServeHTTP(guideProbe, httptest.NewRequest("POST", "/guide/match", strings.NewReader(`{"ai":"estimate"}`)))
	foundGuideRoute := guideProbe.Code == http.StatusOK
	guideTool, foundGuideTool := a.extendRT.tools.Lookup(ctx, tools.Principal{IsOwner: true, Surface: "mcp", Scopes: []string{"full"}}, "livetv_guide_match")
	if !foundGuideRoute || !foundGuideTool || guideTool.ReadOnly {
		t.Fatal("guide matching route lacks an owner-gated MCP tool")
	}
	t.Log("GUIDE_ROUTE_TOOL_COVERAGE PASS: POST /guide/match → livetv_guide_match (owner approval required)")

	// Create local owner account and issue an owner console token.
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := local.CreateOwner(ctx, "owner", "correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownerToken, err := grants.Issue(ctx, ownerID, nil, "owner", core.Device{ID: "console-test"})
	if err != nil {
		t.Fatal(err)
	}

	// Direct or tokenless setup access must not mint owner grants.
	pairingHandler, err := a.buildMCPPairing()
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	pairingHandler.Handler().ServeHTTP(denied, httptest.NewRequest(http.MethodPost, "http://localhost/authorization", strings.NewReader(`{}`)))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("missing owner access: %d", denied.Code)
	}

	// Seed a title, playback history, resolver history trace, and a server log entry.
	movieKey := core.MovieKey(550)
	title := core.Title{
		Key:           movieKey,
		Type:          core.TypeMovie,
		IDs:           core.IDs{TMDB: 550, IMDB: "tt0113277"},
		Title:         "Heat",
		Year:          1995,
		Summary:       "A group of professional bank robbers start to feel the heat from LAPD.",
		ContentRating: "R",
		Rating:        8.3,
		Popularity:    90,
		VoteCount:     5000,
		Released:      time.Date(1995, 12, 15, 0, 0, 0, 0, time.UTC),
	}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if err := a.state.Scrobble(ctx, ownerID, movieKey); err != nil {
		t.Fatal(err)
	}

	trace := resolver.Trace{
		At:             time.Now().Add(-2 * time.Hour).UTC(),
		TitleKey:       movieKey,
		TitleName:      "Heat (1995)",
		Profile:        "Balanced",
		Winner:         "rel-heat-bad",
		SearchDuration: 110 * time.Millisecond,
		TotalDuration:  290 * time.Millisecond,
		Candidates: []resolver.CandidateTrace{
			{
				ReleaseID:     "rel-heat-bad",
				Name:          "Heat.1995.2160p.WEB-DL.DDP5.1.H.265-BADGRP",
				Indexer:       "NZBGeek",
				Score:         94,
				Size:          16 << 30,
				Height:        2160,
				Attempted:     true,
				HealthChecked: true,
				Healthy:       false,
				Reason:        "missing articles on primary provider during stream",
			},
			{
				ReleaseID:     "rel-heat-good",
				Name:          "Heat.1995.1080p.BluRay.x264-SPARKS",
				Indexer:       "DrunkenSlug",
				Score:         88,
				Size:          9 << 30,
				Height:        1080,
				Attempted:     false,
				HealthChecked: true,
				Healthy:       true,
			},
		},
	}
	rawTrace, _ := json.Marshal(trace)
	if _, err := a.db.ExecContext(ctx,
		`INSERT INTO resolver_history(resolved_at, rating_key, title_name, profile, release_id, release_name, indexer, size, error, failover, pinned, candidates, trace)
		 VALUES (?,?,?,?,?,?,?,?,?,0,0,2,?)`,
		trace.At.Unix(), int64(movieKey), trace.TitleName, trace.Profile,
		"rel-heat-bad", "Heat.1995.2160p.WEB-DL.DDP5.1.H.265-BADGRP", "NZBGeek", int64(16<<30),
		"article missing during playback", string(rawTrace)); err != nil {
		t.Fatal(err)
	}
	a.log.Warn("stream stalled on missing article", "title", "Heat", "release", "rel-heat-bad")

	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	// Pair through the public OAuth endpoints and authenticated console API.
	reg, err := http.Post(srv.URL+"/ether/oauth/register", "application/json", strings.NewReader(`{"client_name":"Claude Desktop","redirect_uris":["http://127.0.0.1:9876/cb"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Body.Close()
	var registration struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(reg.Body).Decode(&registration); err != nil || reg.StatusCode != 201 {
		t.Fatalf("registration %d %v", reg.StatusCode, err)
	}
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirect := "http://127.0.0.1:9876/cb"
	params := url.Values{"response_type": {"code"}, "client_id": {registration.ClientID}, "redirect_uri": {redirect}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"full"}, "resource": {srv.URL + "/ether/mcp"}}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	auth, err := noRedirect.Get(srv.URL + "/ether/oauth/authorize?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Body.Close()
	if auth.StatusCode != 303 {
		t.Fatalf("authorize: %d", auth.StatusCode)
	}
	location, err := url.Parse(auth.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"request": location.Query().Get("request"), "approve": true})
	oauthApproveReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/ether/api/agent/mcp/authorization", bytes.NewReader(body))
	oauthApproveReq.Header.Set("Authorization", "Bearer "+ownerToken)
	oauthApproveReq.Header.Set("Content-Type", "application/json")
	oauthApproveReq.Header.Set("X-Ether-Request", "1")
	approval, err := http.DefaultClient.Do(oauthApproveReq)
	if err != nil {
		t.Fatal(err)
	}
	defer approval.Body.Close()
	var approved struct {
		Redirect string `json:"redirect"`
	}
	if err := json.NewDecoder(approval.Body).Decode(&approved); err != nil || approval.StatusCode != 200 {
		t.Fatalf("approval %d %v", approval.StatusCode, err)
	}
	dest, err := url.Parse(approved.Redirect)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {registration.ClientID}, "redirect_uri": {redirect}, "code": {dest.Query().Get("code")}, "code_verifier": {verifier}, "resource": {srv.URL + "/ether/mcp"}}
	exchange, err := http.PostForm(srv.URL+"/ether/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer exchange.Body.Close()
	var issued struct {
		Token string `json:"access_token"`
	}
	if err := json.NewDecoder(exchange.Body).Decode(&issued); err != nil || exchange.StatusCode != 200 {
		t.Fatalf("token %d %v", exchange.StatusCode, err)
	}
	mcpToken := issued.Token

	mcpCall := func(toolName string, args map[string]any) map[string]any {
		t.Helper()
		rpcBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      toolName,
				"arguments": args,
			},
		})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ether/mcp", bytes.NewReader(rpcBody))
		req.Header.Set("Authorization", "Bearer "+mcpToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("MCP tools/call %s HTTP %d: %s", toolName, resp.StatusCode, string(b))
		}
		var out struct {
			Result struct {
				StructuredContent map[string]any `json:"structuredContent"`
				IsError           bool           `json:"isError"`
			} `json:"result"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if out.Error != nil {
			t.Fatalf("MCP tools/call %s RPC error %d: %s", toolName, out.Error.Code, out.Error.Message)
		}
		if out.Result.IsError {
			t.Fatalf("MCP tools/call %s returned tool error: %v", toolName, out.Result.StructuredContent)
		}
		return out.Result.StructuredContent
	}

	// 3. Answer "why did Heat buffer last night" using read tools over /ether/mcp.
	searchRes := mcpCall("catalog_search", map[string]any{"query": "Heat"})
	results, _ := searchRes["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("catalog_search returned no results: %v", searchRes)
	}
	foundKey := results[0].(map[string]any)["key"].(string)
	if foundKey != fmt.Sprint(int64(movieKey)) {
		t.Fatalf("catalog_search key = %q, want %d", foundKey, movieKey)
	}

	histRes := mcpCall("playback_history", map[string]any{"sinceHours": 24})
	entries, _ := histRes["entries"].([]any)
	if len(entries) == 0 {
		t.Fatalf("playback_history returned no entries: %v", histRes)
	}

	traceRes := mcpCall("resolve_trace", map[string]any{"titleKey": foundKey})
	if traceRes["releaseId"] != "rel-heat-bad" {
		t.Fatalf("resolve_trace winner = %v, want rel-heat-bad", traceRes["releaseId"])
	}

	relHealth := mcpCall("release_health", map[string]any{"releaseId": "rel-heat-bad", "titleKey": foundKey})
	if relHealth["status"] != "unhealthy" {
		t.Fatalf("release_health status = %v, want unhealthy (%v)", relHealth["status"], relHealth)
	}

	logsRes := mcpCall("logs_tail", map[string]any{"query": "rel-heat-bad"})
	logLines, _ := logsRes["lines"].([]any)
	if len(logLines) == 0 {
		t.Fatalf("expected logs_tail to find stall log line, got %v", logsRes)
	}

	// 4. Call release_blocklist over MCP: must return pending_approval without blocklisting yet.
	propRes := mcpCall("release_blocklist", map[string]any{
		"releaseId": "rel-heat-bad",
		"reason":    "buffered last night due to missing articles",
	})
	if propRes["status"] != "pending_approval" {
		t.Fatalf("expected pending_approval from release_blocklist, got %v", propRes)
	}
	approvalID, _ := propRes["approvalId"].(string)
	if approvalID == "" {
		t.Fatalf("missing approvalId in %v", propRes)
	}
	if blocked, err := a.resolver.ReleaseBlocked(ctx, "rel-heat-bad"); err != nil || blocked {
		t.Fatalf("release should not be blocked before owner approval (blocked=%v err=%v)", blocked, err)
	}

	// 5. Approve the proposal in the console and verify approval_status + blocklist effect.
	approveReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/ether/api/agent/approvals/"+approvalID,
		strings.NewReader(`{"decision":"approved","note":"Block bad release"}`))
	approveReq.Header.Set("Authorization", "Bearer "+ownerToken)
	approveReq.Header.Set("Content-Type", "application/json")
	approveReq.Header.Set("X-Ether-Request", "1")
	approveResp, err := http.DefaultClient.Do(approveReq)
	if err != nil {
		t.Fatal(err)
	}
	defer approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(approveResp.Body)
		t.Fatalf("POST /ether/api/agent/approvals/%s status = %d: %s", approvalID, approveResp.StatusCode, string(b))
	}

	apprStatus := mcpCall("approval_status", map[string]any{"approvalId": approvalID})
	if apprStatus["status"] != "approved" {
		t.Fatalf("approval_status after approval = %v, want approved", apprStatus)
	}
	if blocked, err := a.resolver.ReleaseBlocked(ctx, "rel-heat-bad"); err != nil || !blocked {
		t.Fatalf("release should be blocked after owner approval (blocked=%v err=%v)", blocked, err)
	}
	// Quality grouping is readable over MCP and changes only after owner approval.
	beforeQuality := a.cfg.Get().Quality
	profileRows := mcpCall("quality_profiles", map[string]any{})["profiles"].([]any)
	if len(profileRows) != len(beforeQuality.Profiles) {
		t.Fatalf("profile count: %v", profileRows)
	}
	var target int = -1
	for i, p := range beforeQuality.Profiles {
		if p.Name == "4K" && p.MediaType == "tv" {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatal("missing default TV profile")
	}
	row := profileRows[target].(map[string]any)
	if row["name"] != "4K" || row["media"] != "tv" || len(row["qualityOrder"].([]any)) == 0 {
		t.Fatalf("profile read: %v", row)
	}
	grouped := []core.QualityItem{{Name: "HD and UHD", Qualities: []string{"webdl-2160", "webdl-1080"}}}
	qualityProposal := mcpCall("quality_order_set", map[string]any{"profile": "4K", "media": "tv", "qualityOrder": grouped})
	if qualityProposal["status"] != "pending_approval" || !reflect.DeepEqual(a.cfg.Get().Quality, beforeQuality) {
		t.Fatal("quality changed before approval")
	}
	qualityApproval := qualityProposal["approvalId"].(string)
	qualityReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/ether/api/agent/approvals/"+qualityApproval, strings.NewReader(`{"decision":"approved"}`))
	qualityReq.Header.Set("Authorization", "Bearer "+ownerToken)
	qualityReq.Header.Set("Content-Type", "application/json")
	qualityReq.Header.Set("X-Ether-Request", "1")
	qualityResp, err := http.DefaultClient.Do(qualityReq)
	if err != nil {
		t.Fatal(err)
	}
	qualityBody, _ := io.ReadAll(qualityResp.Body)
	qualityResp.Body.Close()
	if qualityResp.StatusCode != http.StatusOK {
		t.Fatalf("quality approval: %d %s", qualityResp.StatusCode, qualityBody)
	}
	expectedQuality := beforeQuality
	expectedQuality.Profiles = append([]core.QualityProfile(nil), beforeQuality.Profiles...)
	expectedQuality.Profiles[target].QualityOrder = grouped
	if !reflect.DeepEqual(a.cfg.Get().Quality, expectedQuality) {
		t.Fatalf("approval changed settings beyond the targeted quality order")
	}
	if status := mcpCall("approval_status", map[string]any{"approvalId": qualityApproval}); status["status"] != "approved" {
		t.Fatalf("quality approval status: %v", status)
	}
	// A fresh store proves the approved order survived persistence.
	reopened, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.Get().Quality, expectedQuality) {
		t.Fatal("quality order was not persisted")
	}

	// MCP secret-requiring writes wait for approval and then present a secret card.
	sourceCount := len(a.cfg.Get().Indexers)
	secretProposal := mcpCall("source_add_stremio", map[string]any{})
	if secretProposal["status"] != "pending_approval" {
		t.Fatalf("secret proposal: %v", secretProposal)
	}
	secretApproval := secretProposal["approvalId"].(string)
	var threadID string
	if err := a.db.QueryRowContext(ctx, `SELECT thread_id FROM agent_approvals WHERE id=?`, secretApproval).Scan(&threadID); err != nil {
		t.Fatal(err)
	}
	approveSecret, _ := http.NewRequest(http.MethodPost, srv.URL+"/ether/api/agent/approvals/"+secretApproval, strings.NewReader(`{"decision":"approved"}`))
	approveSecret.Header.Set("Authorization", "Bearer "+ownerToken)
	approveSecret.Header.Set("Content-Type", "application/json")
	approveSecret.Header.Set("X-Ether-Request", "1")
	approveSecretResp, err := http.DefaultClient.Do(approveSecret)
	if err != nil {
		t.Fatal(err)
	}
	approveSecretResp.Body.Close()
	if approveSecretResp.StatusCode != http.StatusOK {
		t.Fatalf("secret approval: %d", approveSecretResp.StatusCode)
	}
	pendingSecretStatus := mcpCall("approval_status", map[string]any{"approvalId": secretApproval})
	if pendingSecretStatus["status"] != "pending_secret" {
		t.Fatalf("source added prematurely: %v", pendingSecretStatus)
	}
	var requestID string
	if err := a.db.QueryRowContext(ctx, `SELECT id FROM agent_secret_requests WHERE thread_id=? AND status='pending'`, threadID).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	if len(a.cfg.Get().Indexers) != sourceCount {
		t.Fatal("source added before secret submission")
	}
	const secretURL = "https://example.invalid/key123/manifest.json"
	secretReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/ether/api/agent/secrets/"+requestID, strings.NewReader(`{"value":"`+secretURL+`"}`))
	secretReq.Header.Set("Authorization", "Bearer "+ownerToken)
	secretReq.Header.Set("Content-Type", "application/json")
	secretReq.Header.Set("X-Ether-Request", "1")
	secretResp, err := http.DefaultClient.Do(secretReq)
	if err != nil {
		t.Fatal(err)
	}
	secretResp.Body.Close()
	if secretResp.StatusCode != http.StatusOK {
		t.Fatalf("secret submit: %d", secretResp.StatusCode)
	}
	if len(a.cfg.Get().Indexers) != sourceCount+1 {
		t.Fatal("source was not added after secret submission")
	}
	storedSecretStatus := mcpCall("approval_status", map[string]any{"approvalId": secretApproval})
	if storedSecretStatus["status"] != "approved" {
		t.Fatalf("source approval after submit: %v", storedSecretStatus)
	}
	var leaked int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_messages WHERE thread_id=? AND parts LIKE ?`, threadID, "%key123%").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("secret in transcript: %d %v", leaked, err)
	}

}

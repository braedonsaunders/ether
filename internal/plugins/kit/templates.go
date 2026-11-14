package kit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/plugins"
)

// TemplateOptions configures a scaffolded plugin project.
type TemplateOptions struct {
	Recipe string // optional catalogue/catalog/job recipe over the integration kind
	Kind   string // source|integration|notification_channel|livetv_source|extension
	Lang   string // ts|js|python
	Dir    string
	ID     string
	Name   string
}

var slugCleanRe = regexp.MustCompile(`[^a-z0-9-]+`)

// Scaffold generates a minimal working plugin for the requested kind and
// language in opts.Dir. Every generated template passes kit.Check out of
// the box.
func Scaffold(opts TemplateOptions) error {
	if opts.Kind == "" {
		opts.Kind = string(plugins.KindLiveTVSource)
	}
	switch plugins.Kind(opts.Kind) {
	case plugins.KindSource, plugins.KindIntegration, plugins.KindNotificationChannel, plugins.KindLiveTVSource, plugins.KindExtension:
	default:
		return fmt.Errorf("unknown plugin kind %q (must be source, integration, notification_channel, livetv_source or extension)", opts.Kind)
	}
	if opts.Lang == "" {
		opts.Lang = "ts"
	}
	switch opts.Lang {
	case "ts", "js", "python":
	default:
		return fmt.Errorf("unknown language %q (must be ts, js or python)", opts.Lang)
	}
	if opts.Dir == "" {
		return fmt.Errorf("target directory is required")
	}
	if entries, err := os.ReadDir(opts.Dir); err == nil {
		if len(entries) != 0 {
			return fmt.Errorf("target directory %q is not empty", opts.Dir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	base := strings.ToLower(filepath.Base(opts.Dir))
	slug := strings.Trim(slugCleanRe.ReplaceAllString(base, "-"), "-")
	if slug == "" || !(slug[0] >= 'a' && slug[0] <= 'z') {
		slug = "sample-" + slug
		slug = strings.Trim(slug, "-")
	}
	if opts.ID == "" {
		opts.ID = "community.example." + strings.ReplaceAll(slug, "-", "")
	}
	if opts.Name == "" {
		opts.Name = humanName(slug)
	}

	files, err := GenerateTemplateFiles(opts)
	if err != nil {
		return err
	}
	for rel, content := range files {
		full := filepath.Join(opts.Dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func humanName(slug string) string {
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if len(p) > 0 {
			if i == 0 {
				parts[i] = strings.ToUpper(p[:1]) + p[1:]
			} else {
				parts[i] = p
			}
		}
	}
	name := strings.Join(parts, " ")
	if name == "" {
		return "Sample plugin"
	}
	return name
}

const defaultIconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">
  <circle cx="32" cy="32" r="26" fill="#14b8a6" />
  <path d="M26 20 L46 32 L26 44 Z" fill="#0f172a" />
</svg>
`

// GenerateTemplateFiles returns the in-memory file map for a scaffolded plugin.
func GenerateTemplateFiles(opts TemplateOptions) (map[string][]byte, error) {
	files := map[string][]byte{
		"assets/icon.svg": []byte(defaultIconSVG),
	}

	isPython := opts.Lang == "python"
	runtimeDecl := map[string]any{
		"kind":  "hosted",
		"entry": "dist/plugin.js",
	}
	if isPython {
		runtimeDecl = map[string]any{
			"kind": "remote",
		}
	}

	var capabilities []string
	var contributes map[string]any
	var stringsEN map[string]string
	var settingsSchema map[string]any
	var fixtures []FixtureCase

	switch plugins.Kind(opts.Kind) {
	case plugins.KindLiveTVSource:
		capabilities = []string{"livetv_channels"}
		contributes = map[string]any{
			"channels": map[string]any{
				"refresh": "6h",
				"test":    true,
			},
		}
		stringsEN = map[string]string{
			"summary":    "Streams live channels with guide data",
			"region":     "Channel region",
			"regionHint": "Pick the lineup region for your household.",
		}
		settingsSchema = map[string]any{
			"fields": []map[string]any{
				{
					"key":     "region",
					"kind":    "select",
					"label":   "$t:region",
					"hint":    "$t:regionHint",
					"default": "us",
					"options": []map[string]any{
						{"value": "us", "label": "United States"},
						{"value": "ca", "label": "Canada"},
					},
				},
			},
		}
		fixtures = []FixtureCase{
			{
				Name:       "health",
				Method:     "GET",
				Path:       "/ether/health",
				WantStatus: 200,
				WantFields: map[string]any{"status": "ok"},
			},
			{
				Name:       "channels",
				Method:     "GET",
				Path:       "/ether/channels.json",
				WantStatus: 200,
			},
			{
				Name:       "guide",
				Method:     "GET",
				Path:       "/ether/guide/ch-1.json",
				Query:      map[string]string{"from": "2026-01-01T00:00:00Z", "to": "2026-01-01T06:00:00Z"},
				WantStatus: 200,
			},
		}

	case plugins.KindSource:
		capabilities = []string{"stream_source"}
		contributes = map[string]any{
			"source": map[string]any{
				"class":     "direct",
				"abilities": []string{"movie", "series"},
			},
		}
		stringsEN = map[string]string{
			"summary":      "Finds playable streams from a configured source",
			"endpoint":     "Source address",
			"endpointHint": "Enter the base address of your stream server.",
		}
		settingsSchema = map[string]any{
			"fields": []map[string]any{
				{
					"key":   "endpoint",
					"kind":  "url",
					"label": "$t:endpoint",
					"hint":  "$t:endpointHint",
				},
			},
		}
		fixtures = []FixtureCase{
			{
				Name:       "health",
				Method:     "GET",
				Path:       "/ether/health",
				WantStatus: 200,
				WantFields: map[string]any{"status": "ok"},
			},
			{
				Name:       "streams",
				Method:     "GET",
				Path:       "/stream/movie/tt0012349.json",
				WantStatus: 200,
			},
		}

	case plugins.KindIntegration:
		capabilities = []string{"list_source", "subtitles"}
		contributes = map[string]any{
			"integration": map[string]any{
				"category": "lists",
				"adds":     []string{"$t:addsCatalog", "$t:addsSubtitles"},
				"connect":  "fields",
				"test":     true,
			},
			"listSources": []map[string]any{
				{
					"key":      "featured",
					"label":    "$t:listLabel",
					"hint":     "$t:listHint",
					"catalog":  "featured",
					"maxItems": 100,
					"refresh":  "6h",
				},
			},
			"subtitles": map[string]any{
				"languages": "any",
				"formats":   []string{"srt", "vtt"},
			},
		}
		stringsEN = map[string]string{
			"summary":       "Adds curated lists and on-demand subtitles",
			"addsCatalog":   "Featured list row for collections and Home",
			"addsSubtitles": "On-demand subtitle lookups when picked",
			"listLabel":     "Featured picks",
			"listHint":      "Refreshes every six hours in the background.",
			"language":      "Preferred subtitle language",
			"languageHint":  "Choose the default language for subtitle searches.",
		}
		settingsSchema = map[string]any{
			"fields": []map[string]any{
				{
					"key":     "language",
					"kind":    "language",
					"label":   "$t:language",
					"hint":    "$t:languageHint",
					"default": "en",
				},
			},
		}
		fixtures = []FixtureCase{
			{
				Name:       "health",
				Method:     "GET",
				Path:       "/ether/health",
				WantStatus: 200,
				WantFields: map[string]any{"status": "ok"},
			},
			{
				Name:       "catalog",
				Method:     "GET",
				Path:       "/catalog/movie/featured.json",
				WantStatus: 200,
			},
			{
				Name:       "subtitles_requires_pick",
				Method:     "POST",
				Path:       "/ether/subtitles/download",
				WantStatus: 403,
			},
			{
				Name:       "subtitles_listing",
				Method:     "GET",
				Path:       "/subtitles/movie/tt0012349/videoHash=0123456789abcdef.json",
				WantStatus: 200,
			},
		}

	case plugins.KindNotificationChannel:
		capabilities = []string{"notify", "events"}
		contributes = map[string]any{
			"notification": map[string]any{
				"sendTest":  true,
				"formats":   []string{"text"},
				"maxLength": 500,
			},
			"events": map[string]any{
				"subscribe": []string{"playback.started", "title.available"},
			},
		}
		stringsEN = map[string]string{
			"summary":   "Delivers server notifications to your webhook",
			"topic":     "Channel topic",
			"topicHint": "Enter the topic name for delivered messages.",
		}
		settingsSchema = map[string]any{
			"fields": []map[string]any{
				{
					"key":      "topic",
					"kind":     "text",
					"label":    "$t:topic",
					"hint":     "$t:topicHint",
					"required": true,
				},
			},
		}
		fixtures = []FixtureCase{
			{
				Name:       "health",
				Method:     "GET",
				Path:       "/ether/health",
				WantStatus: 200,
				WantFields: map[string]any{"status": "ok"},
			},
			{
				Name:       "test",
				Method:     "POST",
				Path:       "/ether/test",
				Body:       json.RawMessage(`{}`),
				WantStatus: 200,
				WantFields: map[string]any{"ok": true},
			},
			{
				Name:       "event",
				Method:     "POST",
				Path:       "/ether/event",
				Body:       json.RawMessage(`{"events":[{"id":"ev-1","name":"playback.started"}]}`),
				WantStatus: 200,
			},
		}

	case plugins.KindExtension:
		if isPython {
			capabilities = []string{"ui", "tools"}
		} else {
			capabilities = []string{"ranking_step", "rule_condition", "ui", "tools"}
		}
		contributes = map[string]any{
			"pages": []map[string]any{
				{
					"key":   "overview",
					"label": "$t:pageTitle",
					"route": "/ether/view/overview",
				},
			},
			"tools": []map[string]any{
				{
					"name":        "status_summary",
					"title":       "$t:toolTitle",
					"description": "Returns a summary of the plugin status.",
					"inputSchema": map[string]any{"type": "object"},
					"readOnly":    true,
				},
			},
		}
		if !isPython {
			contributes["rankingSteps"] = []map[string]any{
				{
					"key":   "preferred_language",
					"label": "$t:rankLabel",
					"hint":  "$t:rankHint",
				},
			}
			contributes["ruleConditions"] = []map[string]any{
				{
					"key":       "has_language",
					"label":     "$t:condLabel",
					"operators": []string{"is", "is_not"},
				},
			}
		}
		stringsEN = map[string]string{
			"summary":       "Ranks releases by audio language and provides an overview page",
			"rankLabel":     "Preferred audio language",
			"rankHint":      "Boosts releases that include your chosen language.",
			"condLabel":     "Audio language",
			"pageTitle":     "Language overview",
			"pageDesc":      "Recent language matches across your library",
			"statLabel":     "Active language",
			"toolTitle":     "Check language status",
			"preferred":     "Preferred language",
			"preferredHint": "Choose the audio language to boost during release ranking.",
		}
		settingsSchema = map[string]any{
			"fields": []map[string]any{
				{
					"key":     "preferred",
					"kind":    "language",
					"label":   "$t:preferred",
					"hint":    "$t:preferredHint",
					"default": "en",
				},
			},
		}
		fixtures = []FixtureCase{
			{
				Name:       "health",
				Method:     "GET",
				Path:       "/ether/health",
				WantStatus: 200,
				WantFields: map[string]any{"status": "ok"},
			},
			{
				Name:       "view_overview",
				Method:     "GET",
				Path:       "/ether/view/overview",
				WantStatus: 200,
			},
			{
				Name:       "tool_status",
				Method:     "POST",
				Path:       "/ether/tool/status_summary",
				Body:       json.RawMessage(`{}`),
				WantStatus: 200,
			},
		}
	}

	manifestMap := map[string]any{
		"id":          opts.ID,
		"version":     "0.1.0",
		"name":        opts.Name,
		"description": stringsEN["summary"] + ".",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         opts.Kind,
			"capabilities": capabilities,
			"summary":      "$t:summary",
			"icon":         "assets/icon.svg",
			"runtime":      runtimeDecl,
			"cluster":      "leader",
			"health":       map[string]any{"interval": "60s"},
			"permissions": map[string]any{
				"network":     []string{},
				"catalog":     "none",
				"collections": "none",
				"folders":     "none",
				"secrets":     []string{},
				"viewers": map[string]any{
					"identity":          "none",
					"watch_history":     "none",
					"write_watch_state": "none",
				},
			},
			"settings":    map[string]any{"$ref": "settings.schema.json"},
			"strings":     map[string]any{"en": "i18n/en.json"},
			"contributes": contributes,
		},
	}

	if opts.Recipe == "catalogue" || opts.Recipe == "catalog" {
		manifestMap["types"] = []string{"movie"}
		manifestMap["resources"] = []string{"catalog"}
		manifestMap["catalogs"] = []map[string]any{{"type": "movie", "id": "featured", "name": "Featured picks"}}
	}
	manBytes, err := json.MarshalIndent(manifestMap, "", "  ")
	if err != nil {
		return nil, err
	}
	if opts.Recipe == "job" {
		manifestMap["ether"].(map[string]any)["schedule"] = []map[string]string{{"every": "1h", "call": "/ether/refresh"}}
		manBytes, err = json.MarshalIndent(manifestMap, "", "  ")
		if err != nil {
			return nil, err
		}
	}
	files["manifest.json"] = append(manBytes, '\n')

	strBytes, err := json.MarshalIndent(stringsEN, "", "  ")
	if err != nil {
		return nil, err
	}
	files["i18n/en.json"] = append(strBytes, '\n')

	schemaBytes, err := json.MarshalIndent(settingsSchema, "", "  ")
	if err != nil {
		return nil, err
	}
	files["settings.schema.json"] = append(schemaBytes, '\n')

	if opts.Recipe == "job" {
		fixtures = append(fixtures, FixtureCase{Name: "refresh", Method: "POST", Path: "/ether/refresh", WantStatus: 200, WantFields: map[string]any{"ok": true}})
	}
	fxBytes, err := json.MarshalIndent(FixtureSuite{Cases: fixtures}, "", "  ")
	if err != nil {
		return nil, err
	}
	files["tests/fixtures.json"] = append(fxBytes, '\n')

	code := generatePluginCode(opts.Lang)
	if opts.Recipe == "catalogue" || opts.Recipe == "catalog" {
		code = strings.ReplaceAll(code, "metas: []", `metas: [{id: "tt0133093", type: "movie", name: "The Matrix"}]`)
		code = strings.ReplaceAll(code, `"metas": []`, `"metas": [{"id": "tt0133093", "type": "movie", "name": "The Matrix"}]`)
	}
	if opts.Recipe == "job" {
		if isPython {
			code = strings.Replace(code, `    if path == "/ether/health":`, `    if path == "/ether/refresh":
        return 200, {"ok": True}
    if path == "/ether/health":`, 1)
		} else {
			code = strings.Replace(code, `  if (p === "/ether/health")`, `  if (p === "/ether/refresh") return jsonResponse(200, { ok: true });
  if (p === "/ether/health")`, 1)
		}
	}
	switch opts.Lang {
	case "ts":
		files["src/index.ts"] = []byte(code)
	case "js":
		files["dist/plugin.js"] = []byte(code)
	case "python":
		files["plugin.py"] = []byte(code)
	}

	files["README.md"] = []byte(generateReadme(opts))
	return files, nil
}

func generatePluginCode(lang string) string {
	if lang == "python" {
		return `#!/usr/bin/env python3
"""Minimal Ether remote sidecar plugin."""

import json
from http.server import BaseHTTPRequestHandler, HTTPServer


def handle_request(method: str, path: str, headers: dict, body: bytes) -> tuple[int, dict]:
    if path == "/ether/health":
        return 200, {"status": "ok"}
    if path == "/ether/configure":
        return 200, {"errors": {}}
    if path == "/ether/test":
        return 200, {"ok": True, "message": "Connection verified"}
    if path == "/ether/channels.json":
        return 200, {
            "channels": [
                {
                    "id": "ch-1",
                    "number": "101",
                    "name": "Sample Channel",
                    "group": "General",
                    "streams": [{"url": "https://example.org/live/stream.m3u8"}],
                }
            ]
        }
    if path.startswith("/ether/guide/"):
        return 200, {"programmes": []}
    if path.startswith("/stream/"):
        return 200, {"streams": []}
    if path.startswith("/catalog/"):
        return 200, {"metas": []}
    if path == "/ether/subtitles/download":
        if not headers.get("X-Ether-Pick") and not headers.get("x-ether-pick"):
            return 403, {"error": {"code": "pick_required", "message": "Subtitle download requires a viewer pick token"}}
        return 200, {"format": "srt", "encoding": "utf8", "data": ""}
    if path.startswith("/subtitles/"):
        return 200, {"subtitles": []}
    if path == "/ether/event":
        payload = json.loads(body.decode("utf-8") or "{}")
        events = payload.get("events", [])
        return 200, {"ack": [e.get("id", "") for e in events if e.get("id")]}
    if path.startswith("/ether/view/"):
        return 200, {
            "spec": {
                "specVersion": 1,
                "layout": "detail",
                "header": [{"kind": "page-header", "title": "$t:pageTitle", "description": "$t:pageDesc"}],
                "body": [{"kind": "stat-tile", "iconKey": "globe", "accent": "teal", "label": "$t:statLabel", "value": {"$": "activeLanguage"}}],
            },
            "data": {"activeLanguage": "en"},
        }
    if path.startswith("/ether/tool/"):
        return 200, {"status": "ok"}
    return 404, {"error": {"code": "not_found", "message": "Route not found"}}


class PluginHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        status, payload = handle_request("GET", self.path.split("?")[0], dict(self.headers), b"")
        self._respond(status, payload)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length > 0 else b""
        status, payload = handle_request("POST", self.path.split("?")[0], dict(self.headers), body)
        self._respond(status, payload)

    def _respond(self, status: int, payload: dict):
        raw = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 8787), PluginHandler).serve_forever()
`
	}

	if lang == "js" {
		return `function jsonResponse(status, data) {
  return {
    status: status,
    headers: { "content-type": "application/json" },
    body: JSON.stringify(data),
  };
}

function handle(req) {
  const p = req.path || "/";
  if (p === "/ether/health") {
    return jsonResponse(200, { status: "ok" });
  }
  if (p === "/ether/configure") {
    return jsonResponse(200, { errors: {} });
  }
  if (p === "/ether/test") {
    return jsonResponse(200, { ok: true, message: "Ready" });
  }
  if (p === "/ether/channels.json") {
    return jsonResponse(200, {
      channels: [
        {
          id: "ch-1",
          number: "101",
          name: "Sample Channel",
          group: "General",
          streams: [{ url: "https://example.org/live/stream.m3u8" }],
        },
      ],
    });
  }
  if (p.indexOf("/ether/guide/") === 0) {
    return jsonResponse(200, { programmes: [] });
  }
  if (p.indexOf("/stream/") === 0) {
    return jsonResponse(200, { streams: [] });
  }
  if (p.indexOf("/catalog/") === 0) {
    return jsonResponse(200, { metas: [] });
  }
  if (p === "/ether/subtitles/download") {
    const hdrs = req.headers || {};
    const pick = hdrs["X-Ether-Pick"] || hdrs["x-ether-pick"];
    if (!pick) return jsonResponse(403, { error: { code: "pick_required", message: "Subtitle download requires a viewer pick token" } });
    return jsonResponse(200, { format: "srt", encoding: "utf8", data: "" });
  }
  if (p.indexOf("/subtitles/") === 0) return jsonResponse(200, { subtitles: [] });
  if (p.indexOf("/ether/markers/") === 0) {
    return jsonResponse(200, { markers: [] });
  }
  if (p === "/ether/event") {
    let payload = {};
    if (typeof req.body === "string" && req.body.length > 0) {
      payload = JSON.parse(req.body);
    } else if (req.body && typeof req.body === "object") {
      payload = req.body;
    }
    const events = Array.isArray(payload.events) ? payload.events : [];
    return jsonResponse(200, { ack: events.map((e) => e.id) });
  }
  if (p.indexOf("/ether/view/") === 0) {
    return jsonResponse(200, {
      spec: {
        specVersion: 1,
        layout: "detail",
        header: [
          {
            kind: "page-header",
            title: "$t:pageTitle",
            description: "$t:pageDesc",
          },
        ],
        body: [
          {
            kind: "stat-tile",
            iconKey: "globe",
            accent: "teal",
            label: "$t:statLabel",
            value: { $: "activeLanguage" },
          },
        ],
      },
      data: { activeLanguage: "English" },
    });
  }
  if (p.indexOf("/ether/action/") === 0) {
    return jsonResponse(200, { refresh: true });
  }
  if (p.indexOf("/ether/tool/") === 0) {
    return jsonResponse(200, { status: "ok" });
  }
  if (p.indexOf("/ether/job/") === 0) {
    return jsonResponse(200, { status: "ok" });
  }
  return jsonResponse(404, {
    error: { code: "not_found", message: "Route not found" },
  });
}

function rank_key(candidate, config) {
  const target = (config && config.preferred) ? String(config.preferred) : "en";
  const langs = Array.isArray(candidate.languages) ? candidate.languages : [];
  return langs.indexOf(target) >= 0 ? 10 : 0;
}

function cond_match(candidate, operator, value) {
  const langs = Array.isArray(candidate.languages) ? candidate.languages : [];
  const has = langs.indexOf(String(value)) >= 0;
  return operator === "is_not" ? !has : has;
}

globalThis.handle = handle;
globalThis.rank_key = rank_key;
globalThis.cond_match = cond_match;
`
	}

	// Default: TypeScript
	return `export interface PluginRequest {
  method: string;
  path: string;
  query?: Record<string, string[]>;
  headers?: Record<string, string>;
  body?: unknown;
}

export interface PluginResponse {
  status: number;
  headers?: Record<string, string>;
  body: string;
}

export interface CandidateFacts {
  releaseId: string;
  name: string;
  resolution?: string;
  languages?: string[];
  cached?: boolean;
  verified?: boolean;
}

function jsonResponse(status: number, data: unknown): PluginResponse {
  return {
    status,
    headers: { "content-type": "application/json" },
    body: JSON.stringify(data),
  };
}

export function handle(req: PluginRequest): PluginResponse {
  const p = req.path || "/";
  if (p === "/ether/health") {
    return jsonResponse(200, { status: "ok" });
  }
  if (p === "/ether/configure") {
    return jsonResponse(200, { errors: {} });
  }
  if (p === "/ether/test") {
    return jsonResponse(200, { ok: true, message: "Ready" });
  }
  if (p === "/ether/channels.json") {
    return jsonResponse(200, {
      channels: [
        {
          id: "ch-1",
          number: "101",
          name: "Sample Channel",
          group: "General",
          streams: [{ url: "https://example.org/live/stream.m3u8" }],
        },
      ],
    });
  }
  if (p.startsWith("/ether/guide/")) {
    return jsonResponse(200, { programmes: [] });
  }
  if (p.startsWith("/stream/")) {
    return jsonResponse(200, { streams: [] });
  }
  if (p.startsWith("/catalog/")) {
    return jsonResponse(200, { metas: [] });
  }
  if (p === "/ether/subtitles/download") {
    const hdrs = req.headers || {};
    const pick = hdrs["X-Ether-Pick"] || hdrs["x-ether-pick"];
    if (!pick) return jsonResponse(403, { error: { code: "pick_required", message: "Subtitle download requires a viewer pick token" } });
    return jsonResponse(200, { format: "srt", encoding: "utf8", data: "" });
  }
  if (p.startsWith("/subtitles/")) return jsonResponse(200, { subtitles: [] });
  if (p.startsWith("/ether/markers/")) {
    return jsonResponse(200, { markers: [] });
  }
  if (p === "/ether/event") {
    let payload: { events?: Array<{ id: string }> } = {};
    if (typeof req.body === "string" && req.body.length > 0) {
      payload = JSON.parse(req.body);
    } else if (req.body && typeof req.body === "object") {
      payload = req.body as { events?: Array<{ id: string }> };
    }
    const events = Array.isArray(payload.events) ? payload.events : [];
    return jsonResponse(200, { ack: events.map((e) => e.id) });
  }
  if (p.startsWith("/ether/view/")) {
    return jsonResponse(200, {
      spec: {
        specVersion: 1,
        layout: "detail",
        header: [
          {
            kind: "page-header",
            title: "$t:pageTitle",
            description: "$t:pageDesc",
          },
        ],
        body: [
          {
            kind: "stat-tile",
            iconKey: "globe",
            accent: "teal",
            label: "$t:statLabel",
            value: { $: "activeLanguage" },
          },
        ],
      },
      data: { activeLanguage: "English" },
    });
  }
  if (p.startsWith("/ether/action/")) {
    return jsonResponse(200, { refresh: true });
  }
  if (p.startsWith("/ether/tool/")) {
    return jsonResponse(200, { status: "ok" });
  }
  if (p.startsWith("/ether/job/")) {
    return jsonResponse(200, { status: "ok" });
  }
  return jsonResponse(404, {
    error: { code: "not_found", message: "Route not found" },
  });
}

export function rank_key(candidate: CandidateFacts, config?: { preferred?: string }): number {
  const target = config?.preferred ? String(config.preferred) : "en";
  const langs = Array.isArray(candidate.languages) ? candidate.languages : [];
  return langs.includes(target) ? 10 : 0;
}

export function cond_match(candidate: CandidateFacts, operator: string, value: unknown): boolean {
  const langs = Array.isArray(candidate.languages) ? candidate.languages : [];
  const has = langs.includes(String(value));
  return operator === "is_not" ? !has : has;
}
`
}

func generateReadme(opts TemplateOptions) string {
	return fmt.Sprintf(`# %s

An Ether plugin (%s, %s).

## Commands

Validate the manifest, copy rules, ViewSpec pages, routes, and hot path:

    ether plugin check .

Run the recorded fixture suite:

    ether plugin test .

Build an installable package:

    ether plugin pack .

Develop against a running Ether server:

    ether plugin dev . --server http://localhost:32400

## Limits

- Only declared network hosts in `+"`manifest.json`"+` are reachable.
- User-visible copy follows Ether sentence-case and length rules.
`, opts.Name, opts.Kind, opts.Lang)
}

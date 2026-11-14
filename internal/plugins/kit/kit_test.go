package kit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/plugins"
)

const goodSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">
  <rect x="6" y="6" width="52" height="52" rx="14" fill="#0f766e"/>
  <circle cx="32" cy="32" r="14" fill="#f8fafc"/>
</svg>`

func baseGoodFiles() ([]byte, map[string][]byte) {
	man := map[string]any{
		"id":      "com.example.good",
		"version": "1.0.0",
		"name":    "Good Plugin",
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "extension",
			"summary":      "Browse curated silent classics and rank preferred audio tracks.",
			"icon":         "assets/icon.svg",
			"capabilities": []string{"list_source", "ranking_step", "rule_condition", "subtitles", "markers", "events", "ui", "tools"},
			"permissions": map[string]any{
				"network": []string{"api.example.com"},
				"catalog": "read",
				"storage": map[string]any{"kv": true},
			},
			"runtime": map[string]any{
				"kind":  "hosted",
				"entry": "dist/plugin.js",
			},
			"settings": map[string]any{
				"$ref": "settings.schema.json",
			},
			"strings": map[string]any{
				"en": "i18n/en.json",
			},
			"contributes": map[string]any{
				"listSources": []map[string]any{
					{"key": "curated", "label": "$t:listLabel", "hint": "$t:listHint", "catalog": "curated"},
				},
				"rankingSteps": []map[string]any{
					{
						"key":    "audio_pref",
						"label":  "$t:rankLabel",
						"hint":   "$t:rankHint",
						"config": map[string]any{"fields": []map[string]any{{"key": "language", "kind": "text", "label": "$t:langLabel"}}},
					},
				},
				"ruleConditions": []map[string]any{
					{"key": "audio_lang", "label": "$t:condLabel", "operators": []string{"is"}, "value": map[string]any{"fields": []map[string]any{{"key": "lang", "kind": "text", "label": "$t:langLabel"}}}},
				},
				"subtitles": map[string]any{
					"languages": []string{"en"},
				},
				"markers": map[string]any{
					"kinds": []string{"intro"},
				},
				"events": map[string]any{
					"subscribe": []string{"playback.started"},
				},
				"pages": []map[string]any{
					{"key": "main", "label": "$t:pageLabel", "location": "nav", "route": "/ether/view/main"},
				},
				"commands": []map[string]any{
					{"key": "refresh", "label": "$t:cmdLabel", "action": "refresh"},
				},
				"tools": []map[string]any{
					{
						"name":        "lookup",
						"title":       "$t:toolTitle",
						"description": "Look up curated entries by title.",
						"readOnly":    true,
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
					},
				},
			},
		},
	}
	manBytes, _ := json.MarshalIndent(man, "", "  ")

	enStrings := map[string]string{
		"listLabel":   "Curated picks",
		"listHint":    "Adds curated silent era films to home rows.",
		"rankLabel":   "Preferred audio track",
		"rankHint":    "Scores releases that include your preferred language.",
		"condLabel":   "Audio language",
		"langLabel":   "Language code",
		"pageLabel":   "Overview",
		"slotLabel":   "Edition badge",
		"cmdLabel":    "Refresh index",
		"toolTitle":   "Search curated films",
		"viewTitle":   "Curated overview",
		"viewHeading": "Recent activity",
		"statLabel":   "Indexed items",
		"enabled":     "Enable sync",
		"enabledHint": "Keeps curated picks updated automatically.",
	}
	enBytes, _ := json.MarshalIndent(enStrings, "", "  ")

	settings := map[string]any{
		"fields": []map[string]any{
			{
				"key":     "enabled",
				"kind":    "bool",
				"label":   "$t:enabled",
				"hint":    "$t:enabledHint",
				"default": true,
			},
		},
	}
	settingsBytes, _ := json.MarshalIndent(settings, "", "  ")

	js := `function handle(req) {
  var p = req && req.path ? req.path : "";
  if (p === "/ether/health") {
    return { status: 200, body: JSON.stringify({ status: "ok" }) };
  }
  if (p === "/ether/configure") {
    return { status: 200, body: JSON.stringify({ ok: true }) };
  }
  if (p.indexOf("/catalog/") === 0) {
    ether.http.fetch({ method: "GET", url: "https://api.example.com/list" });
    return { status: 200, body: JSON.stringify({ metas: [] }) };
  }
  if (p === "/ether/subtitles/download") {
    var hdrs = (req && req.headers) || {};
    var pick = hdrs["X-Ether-Pick"] || hdrs["x-ether-pick"] || "";
    if (!pick) {
      return { status: 403, body: JSON.stringify({ error: { code: "missing_pick", message: "Subtitle download requires an X-Ether-Pick header." } }) };
    }
    return { status: 200, body: JSON.stringify({ format: "srt", encoding: "utf8", data: "" }) };
  }
  if (p.indexOf("/subtitles/") === 0) return { status: 200, body: JSON.stringify({ subtitles: [] }) };
  if (p.indexOf("/ether/markers/") === 0) {
    return { status: 200, body: JSON.stringify({ markers: [] }) };
  }
  if (p === "/ether/event") {
    return { status: 200, body: JSON.stringify({ ack: ["ev-1"] }) };
  }
  if (p.indexOf("/ether/view/") === 0) {
    return {
      status: 200,
      body: JSON.stringify({
        spec: {
          specVersion: 1,
          layout: "detail",
          header: [{ kind: "page-header", title: "$t:viewTitle", description: "$t:viewHeading" }],
          body: [
            { kind: "stat-tile", iconKey: "globe", accent: "teal", label: "$t:statLabel", value: { "$": "count" } }
          ]
        },
        data: { count: "12" }
      })
    };
  }
  return { status: 200, body: JSON.stringify({ ok: true }) };
}

function rank_key(candidate, config) {
  var lang = (config && config.language) ? config.language : "en";
  var langs = (candidate && candidate.languages) || [];
  for (var i = 0; i < langs.length; i++) {
    if (langs[i] === lang) return 100;
  }
  return 0;
}

function cond_match(candidate, operator, value) {
  var langs = (candidate && candidate.languages) || [];
  for (var i = 0; i < langs.length; i++) {
    if (langs[i] === value) return true;
  }
  return false;
}
`
	files := map[string][]byte{
		"assets/icon.svg":      []byte(goodSVG),
		"i18n/en.json":         enBytes,
		"settings.schema.json": settingsBytes,
		"dist/plugin.js":       []byte(js),
	}
	return manBytes, files
}

func TestCheckGoodPluginPasses(t *testing.T) {
	manBytes, files := baseGoodFiles()
	pkg, err := FromFiles(manBytes, files)
	if err != nil {
		t.Fatalf("FromFiles: %v", err)
	}
	rep := Check(context.Background(), pkg)
	if !rep.OK {
		t.Fatalf("expected Check to pass, got errors:\n%s", rep.Text())
	}
	if rep.HotPath == nil || rep.HotPath.Candidates != 10000 || !rep.HotPath.Deterministic {
		t.Fatalf("expected 10k deterministic hot path report, got %+v", rep.HotPath)
	}
	if len(rep.ObservedHosts) != 1 || rep.ObservedHosts[0] != "api.example.com" {
		t.Fatalf("expected observed host api.example.com, got %v", rep.ObservedHosts)
	}
}

func TestCheckAllTemplateKindsAndLangsPass(t *testing.T) {
	kinds := []string{"livetv_source", "source", "integration", "notification_channel", "extension", "catalogue", "job"}
	langs := []string{"ts", "js", "python"}
	for _, k := range kinds {
		for _, l := range langs {
			t.Run(k+"_"+l, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "sample-"+k)
				kind, recipe := k, ""
				if k == "catalogue" || k == "job" {
					kind, recipe = "integration", k
				}
				if err := Scaffold(TemplateOptions{Dir: dir, Kind: kind, Lang: l, Recipe: recipe}); err != nil {
					t.Fatalf("Scaffold(%s, %s): %v", k, l, err)
				}
				pkg, err := Load(dir)
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				rep := Check(context.Background(), pkg)
				if !rep.OK {
					t.Fatalf("template %s/%s failed Check:\n%s", k, l, rep.Text())
				}
				pkgBytes, err := Pack(pkg)
				if err != nil {
					t.Fatalf("Pack(%s/%s): %v", k, l, err)
				}
				reloaded, err := LoadPkgBytes(pkgBytes)
				if err != nil {
					t.Fatalf("LoadPkgBytes: %v", err)
				}
				if k == "livetv_source" && l == "ts" {
					rep2 := Check(context.Background(), reloaded)
					if !rep2.OK {
						t.Fatalf("packed template %s/%s failed Check:\n%s", k, l, rep2.Text())
					}
				}
				tr, err := RunTests(context.Background(), pkg, "")
				if l == "python" {
					if err == nil || !strings.Contains(err.Error(), "remote plugins") {
						t.Fatalf("remote test should explain its runtime requirement: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("RunTests: %v", err)
				}
				if !tr.OK {
					t.Fatalf("template %s/%s failed RunTests:\n%s", k, l, tr.Text())
				}
			})
		}
	}
}

func TestCheckFailureRules(t *testing.T) {
	type testCase struct {
		name     string
		mutate   func(man map[string]any, files map[string][]byte)
		wantRule string
		wantSub  string
	}

	cases := []testCase{
		{
			name: "manifest_kind_capability_mismatch",
			mutate: func(man map[string]any, files map[string][]byte) {
				eth := man["ether"].(map[string]any)
				eth["kind"] = "notification_channel"
			},
			wantRule: "manifest",
			wantSub:  "is not allowed for kind",
		},
		{
			name: "copy_em_dash_in_summary",
			mutate: func(man map[string]any, files map[string][]byte) {
				eth := man["ether"].(map[string]any)
				eth["summary"] = "Curated silent films \u2014 updated daily."
			},
			wantRule: "copy",
			wantSub:  "em or en dashes",
		},
		{
			name: "copy_forbidden_word_in_non_en_locale",
			mutate: func(man map[string]any, files map[string][]byte) {
				eth := man["ether"].(map[string]any)
				strs := eth["strings"].(map[string]any)
				strs["fr"] = "i18n/fr.json"
				files["i18n/fr.json"] = []byte(`{"listLabel":"Simply great films"}`)
			},
			wantRule: "copy",
			wantSub:  "simply, just or easily",
		},
		{
			name: "copy_exclamation_mark_in_locale",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["i18n/en.json"] = []byte(strings.ReplaceAll(string(files["i18n/en.json"]), `"Curated picks"`, `"Curated picks!"`))
			},
			wantRule: "copy",
			wantSub:  "exclamation",
		},
		{
			name: "copy_hint_multi_sentence",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["i18n/en.json"] = []byte(strings.ReplaceAll(
					string(files["i18n/en.json"]),
					`"Adds curated silent era films to home rows."`,
					`"Adds curated films. Works on every row."`,
				))
			},
			wantRule: "copy",
			wantSub:  "one sentence",
		},
		{
			name: "copy_label_exceeds_40_chars",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["i18n/en.json"] = []byte(strings.ReplaceAll(
					string(files["i18n/en.json"]),
					`"Curated picks"`,
					`"Curated silent era motion picture collection items"`,
				))
			},
			wantRule: "copy",
			wantSub:  "longer than 40 characters",
		},
		{
			name: "settings_schema_forbids_json_kind",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["settings.schema.json"] = []byte(`{
					"fields": [
						{"key": "raw", "kind": "json", "label": "$t:enabled"}
					]
				}`)
			},
			wantRule: "settings",
			wantSub:  "json field kind is not allowed",
		},
		{
			name: "settings_schema_folder_requires_permission",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["settings.schema.json"] = []byte(`{
					"fields": [
						{"key": "dir", "kind": "folder", "label": "$t:enabled"}
					]
				}`)
			},
			wantRule: "settings",
			wantSub:  "folders: read",
		},
		{
			name: "icon_illegibility_low_contrast_or_invalid",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["assets/icon.svg"] = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"></svg>`)
			},
			wantRule: "icon",
			wantSub:  "not legible",
		},
		{
			name: "viewspec_rejects_unknown_block_or_class",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["dist/plugin.js"] = []byte(strings.ReplaceAll(
					string(files["dist/plugin.js"]),
					`{ kind: "stat-tile", iconKey: "globe", accent: "teal", label: "$t:statLabel", value: { "$": "count" } }`,
					`{ kind: "stat-tile", iconKey: "globe", accent: "teal", label: "$t:statLabel", value: { "$": "count" }, className: "p-4" }`,
				))
			},
			wantRule: "viewspec",
			wantSub:  "className",
		},
		{
			name: "viewspec_rejects_literal_string_instead_of_t_key",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["dist/plugin.js"] = []byte(strings.ReplaceAll(
					string(files["dist/plugin.js"]),
					`title: "$t:viewTitle"`,
					`title: "Raw English Title"`,
				))
			},
			wantRule: "viewspec_copy",
			wantSub:  "must use a $t:key",
		},
		{
			name: "undeclared_network_host_caught",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["dist/plugin.js"] = []byte(strings.ReplaceAll(
					string(files["dist/plugin.js"]),
					`https://api.example.com/list`,
					`https://evil.unlisted.example.org/steal`,
				))
			},
			wantRule: "permissions",
			wantSub:  "evil.unlisted.example.org",
		},
		{
			name: "subtitle_route_without_pick_must_refuse",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["dist/plugin.js"] = []byte(strings.ReplaceAll(
					string(files["dist/plugin.js"]),
					`if (!pick) {`,
					`if (false) {`,
				))
			},
			wantRule: "subtitles",
			wantSub:  "X-Ether-Pick",
		},
		{
			name: "error_response_bad_shape",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["dist/plugin.js"] = []byte(strings.ReplaceAll(
					string(files["dist/plugin.js"]),
					`{ status: 200, body: JSON.stringify({ status: "ok" }) }`,
					`{ status: 500, body: JSON.stringify({ oops: "broken" }) }`,
				))
			},
			wantRule: "error_shape",
			wantSub:  `{"error":{"code"`,
		},
		{
			name: "hot_path_nondeterministic_rank_key",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["dist/plugin.js"] = []byte(strings.ReplaceAll(
					string(files["dist/plugin.js"]),
					`function rank_key(candidate, config) {`,
					`var __counter = 0; function rank_key(candidate, config) { __counter++; return __counter;`,
				))
			},
			wantRule: "determinism",
			wantSub:  "nondeterministic rank_key",
		},
		{
			name: "hot_path_nondeterministic_cond_match",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["dist/plugin.js"] = []byte(strings.ReplaceAll(
					string(files["dist/plugin.js"]),
					`function cond_match(candidate, operator, value) {`,
					`var __flip = false; function cond_match(candidate, operator, value) { __flip = !__flip; return __flip;`,
				))
			},
			wantRule: "determinism",
			wantSub:  "nondeterministic cond_match",
		},
		{
			name: "hot_path_budget_exceeded",
			mutate: func(man map[string]any, files map[string][]byte) {
				files["dist/plugin.js"] = []byte(strings.ReplaceAll(
					string(files["dist/plugin.js"]),
					`function rank_key(candidate, config) {`,
					`function rank_key(candidate, config) { while (true) {}`,
				))
			},
			wantRule: "hot_path",
			wantSub:  "exceeded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manBytes, files := baseGoodFiles()
			var man map[string]any
			if err := json.Unmarshal(manBytes, &man); err != nil {
				t.Fatalf("unmarshal base manifest: %v", err)
			}
			if !strings.HasPrefix(tc.name, "hot_path_") {
				eth := man["ether"].(map[string]any)
				eth["capabilities"] = []string{"list_source", "subtitles", "markers", "events", "ui", "tools"}
				contrib := eth["contributes"].(map[string]any)
				delete(contrib, "rankingSteps")
				delete(contrib, "ruleConditions")
				// Strip rank_key/cond_match from JS when not testing hot path so LoadJS does not warm a second QuickJS VM.
				if jsIdx := strings.Index(string(files["dist/plugin.js"]), "function rank_key"); jsIdx > 0 {
					files["dist/plugin.js"] = files["dist/plugin.js"][:jsIdx]
				}
			}
			if strings.HasPrefix(tc.name, "manifest_") || strings.HasPrefix(tc.name, "copy_") || strings.HasPrefix(tc.name, "settings_") || strings.HasPrefix(tc.name, "icon_") {
				eth := man["ether"].(map[string]any)
				eth["runtime"] = map[string]any{"kind": "remote", "url": "https://plugin.example.com"}
			}
			tc.mutate(man, files)
			mutManBytes, _ := json.MarshalIndent(man, "", "  ")
			pkg, _ := FromFiles(mutManBytes, files)
			rep := Check(context.Background(), pkg)
			if rep.OK {
				t.Fatalf("expected Check to fail for %s, but it passed", tc.name)
			}
			found := false
			for _, iss := range rep.Errors {
				if iss.Rule == tc.wantRule && strings.Contains(iss.Message, tc.wantSub) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected error with rule=%q containing %q, got:\n%s", tc.wantRule, tc.wantSub, rep.Text())
			}
		})
	}
}

func TestPackDigestTamperDetection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "livetv")
	if err := Scaffold(TemplateOptions{Dir: dir, Kind: "livetv_source", Lang: "ts"}); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	pkg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkgBytes, err := Pack(pkg)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	outFile := filepath.Join(t.TempDir(), "out.etherpkg")
	if err := os.WriteFile(outFile, pkgBytes, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	loaded, err := Load(outFile)
	if err != nil {
		t.Fatalf("Load(.etherpkg): %v", err)
	}
	if loaded.Manifest.ID != pkg.Manifest.ID {
		t.Fatalf("loaded ID = %q, want %q", loaded.Manifest.ID, pkg.Manifest.ID)
	}

	// Verify plugins.PkgDigest matches what Pack wrote.
	wantDigest := plugins.PkgDigest(loaded.Files)
	gotDigest := strings.TrimSpace(string(loaded.Files["digest"]))
	if gotDigest != wantDigest {
		t.Fatalf("digest mismatch: got %q, want %q", gotDigest, wantDigest)
	}
}

func TestScaffoldDoesNotOverwriteExistingFiles(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(name, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Scaffold(TemplateOptions{Kind: "livetv_source", Dir: dir}); err == nil {
		t.Fatal("expected nonempty directory to be refused")
	}
	data, err := os.ReadFile(name)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing file changed: %q, %v", data, err)
	}
}

func TestRemoteFixturesUseRunningServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ether/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	t.Setenv("ETHER_PLUGIN_URL", server.URL)
	files, err := GenerateTemplateFiles(TemplateOptions{Kind: "livetv_source", Lang: "python", ID: "com.example.remote", Name: "Remote"})
	if err != nil {
		t.Fatal(err)
	}
	files["tests/fixtures.json"] = []byte(`{"cases":[{"name":"health","path":"/ether/health","wantStatus":200,"wantFields":{"status":"ok"}}]}`)
	pkg, err := FromFiles(files["manifest.json"], files)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunTests(context.Background(), pkg, "")
	if err != nil || !report.OK || len(report.Cases) != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

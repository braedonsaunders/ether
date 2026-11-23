package catalogue

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/store"
)

func makePkg(t *testing.T, id, name, version string, networkHosts []string) ([]byte, string) {
	t.Helper()
	man := map[string]any{
		"id":          id,
		"name":        name,
		"version":     version,
		"description": "Free live television channels.",
		"resources":   []string{"catalog", "stream"},
		"types":       []string{"tv"},
		"ether": map[string]any{
			"protocol":     1,
			"kind":         "livetv_source",
			"summary":      "Free live television channels.",
			"capabilities": []string{"livetv_channels"},
			"runtime": map[string]any{
				"kind":  "hosted",
				"entry": "plugin.js",
			},
			"permissions": map[string]any{
				"network": networkHosts,
			},
			"contributes": map[string]any{
				"channels": map[string]any{
					"test": true,
				},
			},
		},
	}
	manBytes, err := json.Marshal(man)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	jsBytes := []byte("export default {};\n")
	files := map[string][]byte{
		"manifest.json": manBytes,
		"plugin.js":     jsBytes,
	}
	digest := plugins.PkgDigest(files)
	files["digest"] = []byte(digest + "\n")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, k := range []string{"manifest.json", "plugin.js", "digest"} {
		w, err := zw.Create(k)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(files[k]); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	pkgBytes := buf.Bytes()
	sum := sha256.Sum256(pkgBytes)
	return pkgBytes, hex.EncodeToString(sum[:])
}

func TestSignAndVerifyDetached(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	_, sha := makePkg(t, "community.plutotv.ether", "Pluto TV", "1.0.0", []string{"api.pluto.tv"})
	idx := Index{
		Schema:      1,
		GeneratedAt: "2026-11-02T00:00:00Z",
		Entries: []Entry{
			{
				ID:           "community.plutotv.ether",
				Name:         "Pluto TV",
				Summary:      "Free live television channels.",
				Kind:         "livetv_source",
				Category:     "Live TV",
				Capabilities: []string{"livetv_channels"},
				Permissions: plugins.Permissions{
					Network: []string{"api.pluto.tv"},
				},
				Versions: []Version{
					{
						Version: "1.0.0",
						URL:     "https://catalogue.example.invalid/pluto-1.0.0.etherpkg",
						SHA256:  sha,
					},
				},
			},
		},
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	sig := SignDetached(raw, priv)

	parsed, err := ParseAndVerify(raw, []byte(sig), pub)
	if err != nil {
		t.Fatalf("ParseAndVerify failed: %v", err)
	}
	if len(parsed.Entries) != 1 || parsed.Entries[0].ID != "community.plutotv.ether" {
		t.Fatalf("unexpected parsed entries: %+v", parsed.Entries)
	}

	// Tampered index payload must be rejected.
	tampered := bytes.Replace(raw, []byte("Pluto TV"), []byte("Evil  TV"), 1)
	if _, err := ParseAndVerify(tampered, []byte(sig), pub); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on tampered index, got %v", err)
	}

	// Wrong public key must be rejected.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := ParseAndVerify(raw, []byte(sig), otherPub); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid with wrong public key, got %v", err)
	}
}

func TestLoadCatalogueTemplateDir(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "sdk", "catalogue-template", "examples")
	idx, err := LoadEntriesDir(dir)
	if err != nil {
		t.Fatalf("LoadEntriesDir(%s): %v", dir, err)
	}
	if len(idx.Entries) != 2 {
		t.Fatalf("expected 2 template entries, got %d", len(idx.Entries))
	}
	byID := map[string]Entry{}
	for _, e := range idx.Entries {
		byID[e.ID] = e
	}
	rec, ok := byID["community.example.recordings"]
	if !ok || rec.Kind != string(plugins.KindContent) || rec.Permissions.Sources != "search" {
		t.Fatalf("unexpected content template entry: %+v", rec)
	}
	wrapped, ok := byID["community.example.wrapped"]
	if !ok || wrapped.Permissions.Viewers.WatchHistory != "granted_viewers" {
		t.Fatalf("watch history permission lost: %+v", wrapped)
	}
}

func TestCatalogueInstallRevokeAndAutoUpdate(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)

	var noticesMu sync.Mutex
	var notices []string

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	pkgV1, shaV1 := makePkg(t, "community.plutotv.ether", "Pluto TV", "1.0.0", []string{"api.pluto.tv"})
	pkgV2SamePerms, shaV2Same := makePkg(t, "community.plutotv.ether", "Pluto TV", "1.1.0", []string{"api.pluto.tv"})
	pkgV3MorePerms, shaV3More := makePkg(t, "community.plutotv.ether", "Pluto TV", "1.2.0", []string{"api.pluto.tv", "ads.evil.example"})

	var srvMu sync.Mutex
	var currentIndex []byte
	var currentSig string
	tamperPkg := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvMu.Lock()
		defer srvMu.Unlock()
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write(currentIndex)
		case "/index.json.sig":
			_, _ = w.Write([]byte(currentSig))
		case "/pluto-1.0.0.etherpkg":
			if tamperPkg {
				bad := append([]byte(nil), pkgV1...)
				bad[len(bad)-1] ^= 0xff
				_, _ = w.Write(bad)
				return
			}
			_, _ = w.Write(pkgV1)
		case "/pluto-1.1.0.etherpkg":
			_, _ = w.Write(pkgV2SamePerms)
		case "/pluto-1.2.0.etherpkg":
			_, _ = w.Write(pkgV3MorePerms)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	generation := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	publishIndex := func(idx Index) {
		t.Helper()
		generation = generation.Add(time.Second)
		idx.GeneratedAt = generation.Format(time.RFC3339Nano)
		raw, err := json.Marshal(idx)
		if err != nil {
			t.Fatalf("marshal index: %v", err)
		}
		sig := SignDetached(raw, priv)
		srvMu.Lock()
		currentIndex = raw
		currentSig = sig
		srvMu.Unlock()
	}

	publishIndex(Index{
		Schema:      1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Entries: []Entry{
			{
				ID:           "community.plutotv.ether",
				Name:         "Pluto TV",
				Summary:      "Free live television channels.",
				Kind:         "livetv_source",
				Category:     "Live TV",
				Capabilities: []string{"livetv_channels"},
				Permissions: plugins.Permissions{
					Network: []string{"api.pluto.tv"},
				},
				Versions: []Version{
					{
						Version: "1.0.0",
						URL:     srv.URL + "/pluto-1.0.0.etherpkg",
						SHA256:  shaV1,
					},
				},
			},
		},
	})

	mgr, err := plugins.New(plugins.Deps{
		DB:          db,
		Registry:    extend.New(),
		Tools:       tools.NewCatalogue(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		FetchClient: srv.Client(),
		Backends: plugins.Backends{
			Notice: func(text string) {
				noticesMu.Lock()
				notices = append(notices, text)
				noticesMu.Unlock()
			},
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	cat, err := New(Options{
		DB:        db,
		Manager:   mgr,
		IndexURL:  srv.URL + "/index.json",
		PublicKey: pubB64,
		Client:    srv.Client(),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new catalogue: %v", err)
	}
	mgr.SetCatalogue(cat)

	// 1. Refresh and query via HTTP handler.
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	apiSrv := httptest.NewServer(mgr.Handler())
	defer apiSrv.Close()

	resp, err := http.Get(apiSrv.URL + "/catalogue?q=pluto&kind=livetv_source")
	if err != nil {
		t.Fatalf("GET /catalogue: %v", err)
	}
	var catResp struct {
		Configured bool            `json:"configured"`
		Entries    []CatalogueItem `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catResp); err != nil {
		t.Fatalf("decode /catalogue: %v", err)
	}
	_ = resp.Body.Close()
	if !catResp.Configured || len(catResp.Entries) != 1 {
		t.Fatalf("expected 1 configured catalogue entry, got %+v", catResp)
	}
	if catResp.Entries[0].LatestVersion != "1.0.0" || catResp.Entries[0].Installed {
		t.Fatalf("unexpected entry state: %+v", catResp.Entries[0])
	}

	// 2. Digest mismatch at install must be rejected.
	srvMu.Lock()
	tamperPkg = true
	srvMu.Unlock()
	_, err = mgr.PreviewInstall(ctx, plugins.Source{Kind: "catalogue", ID: "community.plutotv.ether"})
	if !errors.Is(err, plugins.ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch when artifact is tampered, got %v", err)
	}
	srvMu.Lock()
	tamperPkg = false
	srvMu.Unlock()

	// A digest alone cannot turn unreviewed permissions into verified access.
	var initial Index
	if err := json.Unmarshal(currentIndex, &initial); err != nil {
		t.Fatal(err)
	}
	underReviewed := initial
	underReviewed.Entries = append([]Entry(nil), initial.Entries...)
	underReviewed.Entries[0].Permissions.Network = nil
	publishIndex(underReviewed)
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "catalogue", ID: "community.plutotv.ether"}); err == nil || !strings.Contains(err.Error(), "beyond the catalogue review") {
		t.Fatalf("unreviewed permissions accepted: %v", err)
	}
	publishIndex(initial)
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	preview, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "catalogue", ID: "community.plutotv.ether"})
	if err != nil {
		t.Fatal(err)
	}
	changed := initial
	changed.Entries = append([]Entry(nil), initial.Entries...)
	changed.Entries[0].Versions = append([]Version(nil), initial.Entries[0].Versions...)
	changed.Entries[0].Versions[0].SHA256 = strings.Repeat("a", 64)
	publishIndex(changed)
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Install(ctx, preview, plugins.InstallOptions{}); err == nil {
		t.Fatal("stale preview installed after its signed digest changed")
	}
	publishIndex(initial)
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	// 3. Install from catalogue via HTTP API sets Verified: true and AutoUpdate: true.
	installBody := `{"kind":"catalogue","id":"community.plutotv.ether","autoUpdate":true}`
	instResp, err := http.Post(apiSrv.URL+"/install", "application/json", strings.NewReader(installBody))
	if err != nil {
		t.Fatalf("POST /install: %v", err)
	}
	if instResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", instResp.StatusCode)
	}
	var summary plugins.Summary
	if err := json.NewDecoder(instResp.Body).Decode(&summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	_ = instResp.Body.Close()
	if !summary.Verified {
		t.Fatalf("expected catalogue install to mark plugin Verified=true")
	}
	if summary.Version != "1.0.0" || !summary.AutoUpdate {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	// 4. Publish 1.1.0 with identical permissions; Refresh auto-updates to 1.1.0 and records PrevVersion=1.0.0.
	publishIndex(Index{
		Schema:      1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Entries: []Entry{
			{
				ID:           "community.plutotv.ether",
				Name:         "Pluto TV",
				Summary:      "Free live television channels.",
				Kind:         "livetv_source",
				Category:     "Live TV",
				Capabilities: []string{"livetv_channels"},
				Permissions: plugins.Permissions{
					Network: []string{"api.pluto.tv"},
				},
				Versions: []Version{
					{Version: "1.0.0", URL: srv.URL + "/pluto-1.0.0.etherpkg", SHA256: shaV1},
					{Version: "1.1.0", URL: srv.URL + "/pluto-1.1.0.etherpkg", SHA256: shaV2Same},
				},
			},
		},
	})
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatalf("refresh 1.1.0: %v", err)
	}
	rec, err := mgr.Get(ctx, "community.plutotv.ether")
	if err != nil {
		t.Fatalf("get after auto-update: %v", err)
	}
	if rec.Version != "1.1.0" || rec.PrevVersion != "1.0.0" {
		t.Fatalf("expected auto-update to 1.1.0 with prev 1.0.0, got version=%s prev=%s", rec.Version, rec.PrevVersion)
	}
	if !rec.AutoUpdate || !rec.Verified {
		t.Fatalf("expected AutoUpdate and Verified preserved after auto-update, got %+v", rec)
	}

	// 5. One-step rollback returns to 1.0.0; then re-apply update to 1.1.0.
	rolled, err := mgr.Rollback(ctx, "community.plutotv.ether")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rolled.Version != "1.0.0" {
		t.Fatalf("expected rollback to 1.0.0, got %s", rolled.Version)
	}
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatalf("refresh after rollback: %v", err)
	}

	// 6. Publish 1.2.0 that adds a network host; auto-update must pause and wait for owner confirmation.
	publishIndex(Index{
		Schema:      1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Entries: []Entry{
			{
				ID:           "community.plutotv.ether",
				Name:         "Pluto TV",
				Summary:      "Free live television channels.",
				Kind:         "livetv_source",
				Category:     "Live TV",
				Capabilities: []string{"livetv_channels"},
				Permissions: plugins.Permissions{
					Network: []string{"api.pluto.tv", "ads.evil.example"},
				},
				Versions: []Version{
					{Version: "1.0.0", URL: srv.URL + "/pluto-1.0.0.etherpkg", SHA256: shaV1},
					{Version: "1.1.0", URL: srv.URL + "/pluto-1.1.0.etherpkg", SHA256: shaV2Same},
					{Version: "1.2.0", URL: srv.URL + "/pluto-1.2.0.etherpkg", SHA256: shaV3More},
				},
			},
		},
	})
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatalf("refresh 1.2.0: %v", err)
	}
	rec, err = mgr.Get(ctx, "community.plutotv.ether")
	if err != nil {
		t.Fatalf("get after 1.2.0 refresh: %v", err)
	}
	if rec.Version != "1.1.0" || rec.UpdateVersion != "1.2.0" {
		t.Fatalf("expected update paused at 1.1.0 with pending 1.2.0, got version=%s updateVersion=%s", rec.Version, rec.UpdateVersion)
	}
	// A pending update cannot be applied after its version is removed from the index.
	var withUpdate Index
	if err := json.Unmarshal(currentIndex, &withUpdate); err != nil {
		t.Fatal(err)
	}
	withoutUpdate := withUpdate
	withoutUpdate.Entries = append([]Entry(nil), withUpdate.Entries...)
	withoutUpdate.Entries[0].Versions = append([]Version(nil), withUpdate.Entries[0].Versions[:2]...)
	publishIndex(withoutUpdate)
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ApplyUpdate(ctx, "community.plutotv.ether", true); err == nil {
		t.Fatal("removed catalogue version was applied")
	}
	publishIndex(withUpdate)
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	// Applying without accepting added permissions must fail.
	if _, err := mgr.ApplyUpdate(ctx, "community.plutotv.ether", false); err == nil {
		t.Fatalf("expected ApplyUpdate without acceptAdded to fail")
	}
	// Applying with acceptAdded=true succeeds.
	updated, err := mgr.ApplyUpdate(ctx, "community.plutotv.ether", true)
	if err != nil {
		t.Fatalf("ApplyUpdate with acceptAdded=true: %v", err)
	}
	if updated.Version != "1.2.0" {
		t.Fatalf("expected version 1.2.0 after ApplyUpdate, got %s", updated.Version)
	}

	// 7. Revoking the entry disables the installed plugin, records the reason, and emits a console notice.
	revokeReason := "Revoked due to upstream credential harvesting."
	publishIndex(Index{
		Schema:      1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Entries: []Entry{
			{
				ID:      "community.plutotv.ether",
				Name:    "Pluto TV",
				Summary: "Free live television channels.",
				Kind:    "livetv_source",
				Revoked: true,
				Reason:  revokeReason,
				Versions: []Version{
					{Version: "1.2.0", URL: srv.URL + "/pluto-1.2.0.etherpkg", SHA256: shaV3More},
				},
			},
		},
	})
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatalf("refresh revoked: %v", err)
	}
	rec, err = mgr.Get(ctx, "community.plutotv.ether")
	if err != nil {
		t.Fatalf("get after revoke: %v", err)
	}
	if rec.Enabled {
		t.Fatalf("expected revoked plugin to be disabled")
	}

	detailResp, err := http.Get(apiSrv.URL + "/community.plutotv.ether")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	var detail plugins.Detail
	if err := json.NewDecoder(detailResp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	_ = detailResp.Body.Close()
	if detail.Health != "disabled" || !strings.Contains(detail.HealthDetail.Reason, revokeReason) {
		t.Fatalf("expected disabled health with revoke reason, got health=%q reason=%q", detail.Health, detail.HealthDetail.Reason)
	}

	noticesMu.Lock()
	gotNotices := append([]string(nil), notices...)
	noticesMu.Unlock()
	foundRevokeNotice := false
	for _, msg := range gotNotices {
		if strings.Contains(msg, "Pluto TV") && strings.Contains(msg, revokeReason) {
			foundRevokeNotice = true
		}
	}
	if !foundRevokeNotice {
		t.Fatalf("expected revoked notice with reason, got %+v", gotNotices)
	}

	// Simulate a crash after saving a revocation but before disabling the plugin.
	if _, err := db.Exec(`UPDATE plugins_installed SET enabled = 1 WHERE id = ?`, "community.plutotv.ether"); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Options{DB: db, Manager: mgr, IndexURL: srv.URL + "/index.json", PublicKey: pubB64, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.EnforceCachedRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	rec, err = mgr.Get(ctx, "community.plutotv.ether")
	if err != nil || rec.Enabled {
		t.Fatalf("cached revocation not enforced: enabled=%v err=%v", rec.Enabled, err)
	}

	// Re-enabling a revoked plugin must be refused with ErrRevoked.
	if err := mgr.SetEnabled(ctx, "community.plutotv.ether", true); !errors.Is(err, plugins.ErrRevoked) {
		t.Fatalf("expected ErrRevoked when re-enabling revoked plugin, got %v", err)
	}
}

func TestCacheRequiresCurrentSigningKey(t *testing.T) {
	db := store.OpenTest(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schema":1,"entries":[]}`)
	cache := cacheStore{db: db}
	if err := cache.save(context.Background(), Index{Schema: 1}, raw, SignDetached(raw, priv), time.Now(), "https://example.invalid/index.json", pub); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := cache.load(context.Background(), pub, "https://example.invalid/index.json"); !ok {
		t.Fatal("valid cached index rejected")
	}
	if _, _, ok := cache.load(context.Background(), pub, "https://other.invalid/index.json"); ok {
		t.Fatal("cached index trusted for another source")
	}
	other, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := cache.load(context.Background(), other, "https://example.invalid/index.json"); ok {
		t.Fatal("cached index trusted under a different key")
	}
	if _, err := db.Exec(`UPDATE plugins_catalogue_index SET raw_json = '{"schema":1,"entries":[] }'`); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := cache.load(context.Background(), pub, "https://example.invalid/index.json"); ok {
		t.Fatal("tampered cached index trusted")
	}
}

func TestHexKeysAndSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	decodedPub, err := ParsePublicKey(hex.EncodeToString(pub))
	if err != nil || !bytes.Equal(decodedPub, pub) {
		t.Fatalf("hex public key: %v", err)
	}
	decodedPriv, err := ParsePrivateKey(hex.EncodeToString(priv))
	if err != nil || !bytes.Equal(decodedPriv, priv) {
		t.Fatalf("hex private key: %v", err)
	}
	raw := []byte(`{"schema":1,"entries":[]}`)
	if err := VerifyDetached(raw, []byte(hex.EncodeToString(ed25519.Sign(priv, raw))), pub); err != nil {
		t.Fatal(err)
	}
}

func TestLatestIncludesPrereleaseOrdering(t *testing.T) {
	e := Entry{Versions: []Version{{Version: "1.0.0-beta.2"}, {Version: "1.0.0-beta.10"}, {Version: "1.0.0"}}}
	v, ok := e.Latest()
	if !ok || v.Version != "1.0.0" {
		t.Fatalf("latest = %q", v.Version)
	}
	if plugins.CompareSemVer("1.0.0-beta.2", "1.0.0-beta.10") >= 0 {
		t.Fatal("numeric prerelease order is wrong")
	}
	if plugins.CompareSemVer("1.0.0", "1.0.0-rc.1") <= 0 {
		t.Fatal("release must sort after prerelease")
	}
}

func TestLatestVersionReviewedPermissionsDisplayed(t *testing.T) {
	permissions := plugins.Permissions{Network: []string{"latest.example"}}
	s := &Service{idx: Index{Entries: []Entry{{ID: "test", Name: "Test", Kind: "extension", Versions: []Version{{Version: "1.0.0", Permissions: &permissions}}}}}}
	items := s.Search(context.Background(), Filter{})
	if len(items) != 1 || len(items[0].PermissionLines) != 1 || !strings.Contains(items[0].PermissionLines[0], "latest.example") {
		t.Fatalf("incorrect reviewed permission lines: %+v", items)
	}
}

func TestSignatureURLPreservesQuery(t *testing.T) {
	s := &Service{indexURL: "https://catalogue.example/index.json?token=value"}
	if got := s.sigURL(); got != "https://catalogue.example/index.json.sig?token=value" {
		t.Fatalf("signature URL = %q", got)
	}
}

func TestSignedIndexCannotRollBackAfterRestart(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{DB: db, IndexURL: "https://catalogue.example/index.json", PublicKey: base64.StdEncoding.EncodeToString(pub)}
	service, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	newer, _ := json.Marshal(Index{Schema: 1, GeneratedAt: "2026-11-02T12:00:00Z", Entries: []Entry{}})
	older, _ := json.Marshal(Index{Schema: 1, GeneratedAt: "2026-11-01T12:00:00Z", Entries: []Entry{}})
	if _, err := service.Ingest(ctx, newer, []byte(SignDetached(newer, priv))); err != nil {
		t.Fatal(err)
	}
	service, err = New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ingest(ctx, older, []byte(SignDetached(older, priv))); err == nil {
		t.Fatal("accepted an older signed index")
	}
	missing, _ := json.Marshal(Index{Schema: 1, Entries: []Entry{}})
	if _, err := service.Ingest(ctx, missing, []byte(SignDetached(missing, priv))); err == nil {
		t.Fatal("accepted a timestamp-free index")
	}
	if _, err := service.Ingest(ctx, newer, []byte(SignDetached(newer, priv))); err != nil {
		t.Fatalf("identical index rejected: %v", err)
	}
	other := []byte(`{"schema":1,"generatedAt":"2026-11-02T12:00:00Z","entries":null}`)
	if _, err := service.Ingest(ctx, other, []byte(SignDetached(other, priv))); err == nil {
		t.Fatal("accepted conflicting index with same timestamp")
	}
}

func TestEntryIconReturnsActiveCatalogueIconOnly(t *testing.T) {
	s := &Service{idx: Index{Entries: []Entry{
		{ID: "community.plutotv.ether", Icon: "https://catalogue.example/icons/pluto.svg"},
		{ID: "community.revoked.ether", Icon: "https://catalogue.example/icons/revoked.svg", Revoked: true},
	}}}
	if got := s.EntryIcon("community.plutotv.ether"); got != "https://catalogue.example/icons/pluto.svg" {
		t.Fatalf("EntryIcon(active) = %q", got)
	}
	if got := s.EntryIcon("community.revoked.ether"); got != "" {
		t.Fatalf("EntryIcon(revoked) = %q, want empty", got)
	}
	if got := s.EntryIcon("community.unknown.ether"); got != "" {
		t.Fatalf("EntryIcon(unknown) = %q, want empty", got)
	}
}

func TestCatalogueContentPluginWithLibraryInstalls(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	manifest := `{
		"id": "community.music.ether",
		"name": "Music",
		"version": "1.0.0",
		"ether": {
			"protocol": 1,
			"kind": "content",
			"summary": "Curated music library.",
			"runtime": { "kind": "hosted", "entry": "plugin.js" },
			"capabilities": ["catalog_provider", "kind_resolver"],
			"permissions": { "network": ["api.example.com"], "sources": "search" },
			"contributes": {
				"mediaKinds": [{
					"key": "music",
					"label": "Music",
					"library": { "name": "Music" },
					"levels": [
						{ "key": "album", "label": "Album", "container": true },
						{ "key": "track", "label": "Track", "playable": true }
					],
					"sync": { "mode": "pull", "refresh": "1h" }
				}]
			}
		}
	}`
	files := map[string][]byte{
		"manifest.json": []byte(manifest),
		"plugin.js":     []byte(`export default {};`),
	}
	files["digest"] = []byte(plugins.PkgDigest(files) + "\n")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, k := range []string{"manifest.json", "plugin.js", "digest"} {
		w, err := zw.Create(k)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[k]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	pkgBytes := buf.Bytes()
	sum := sha256.Sum256(pkgBytes)
	digest := hex.EncodeToString(sum[:])

	var srvURL string
	var currentIndex, currentSig []byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write(currentIndex)
		case "/index.json.sig":
			_, _ = w.Write(currentSig)
		case "/music.etherpkg":
			_, _ = w.Write(pkgBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	publish := func(idx Index) {
		raw, err := json.Marshal(idx)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		currentIndex = raw
		currentSig = []byte(SignDetached(raw, priv))
		mu.Unlock()
	}

	baseEntry := Entry{
		ID:           "community.music.ether",
		Name:         "Music",
		Summary:      "Curated music library.",
		Kind:         "content",
		Capabilities: []string{"catalog_provider", "kind_resolver"},
		Permissions:  plugins.Permissions{Network: []string{"api.example.com"}, Sources: "search"},
		Versions: []Version{{
			Version: "1.0.0",
			URL:     srvURL + "/music.etherpkg",
			SHA256:  digest,
		}},
	}
	publish(Index{Schema: 1, GeneratedAt: "2026-11-23T12:00:00Z", Entries: []Entry{baseEntry}})

	db := store.OpenTest(t)
	mgr, err := plugins.New(plugins.Deps{
		DB:          db,
		FetchClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := New(Options{
		DB:        db,
		Manager:   mgr,
		IndexURL:  srvURL + "/index.json",
		PublicKey: base64.StdEncoding.EncodeToString(pub),
		Client:    srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetCatalogue(cat)
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	// Preview + Install must succeed for a reviewed content plugin declaring a library.
	preview, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "catalogue", ID: "community.music.ether"})
	if err != nil {
		t.Fatalf("PreviewInstall failed: %v", err)
	}
	rec, err := mgr.Install(ctx, preview, plugins.InstallOptions{})
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if rec.ID != "community.music.ether" || !rec.Verified {
		t.Fatalf("unexpected installed record: %+v", rec)
	}

	// Re-preview cleanly, then drop an approved capability in the signed index.
	preview2, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "catalogue", ID: "community.music.ether"})
	if err != nil {
		t.Fatalf("PreviewInstall (2) failed: %v", err)
	}
	underReviewed := baseEntry
	underReviewed.Capabilities = []string{"catalog_provider"}
	publish(Index{Schema: 1, GeneratedAt: "2026-11-23T12:01:00Z", Entries: []Entry{underReviewed}})
	if _, err := cat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Install(ctx, preview2, plugins.InstallOptions{}); err == nil || !strings.Contains(err.Error(), "catalogue review changed") {
		t.Fatalf("expected Install to reject dropped capability, got: %v", err)
	}
	if _, err := mgr.PreviewInstall(ctx, plugins.Source{Kind: "catalogue", ID: "community.music.ether"}); err == nil || !strings.Contains(err.Error(), "beyond the catalogue review") {
		t.Fatalf("expected PreviewInstall to reject unreviewed capability, got: %v", err)
	}
}

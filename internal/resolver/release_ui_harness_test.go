package resolver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// harnessOwner is a loopback test token, not a Plex credential.
const harnessOwner = "harness-owner"

// TestReleaseUIHarness serves the embedded admin UI and the real release
// API on 127.0.0.1 only. It uses a temp config, an offline indexer, and
// one fixture title. Set ETHER_RELEASE_HARNESS=1 to run it.
func TestReleaseUIHarness(t *testing.T) {
	if os.Getenv("ETHER_RELEASE_HARNESS") != "1" {
		t.Skip("set ETHER_RELEASE_HARNESS=1 to serve the loopback release UI")
	}
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Plex.Claimed = true
		c.Plex.Token = harnessOwner
		c.Plex.OwnerUsername = "owner"
		c.Server.FriendlyName = "Loopback"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	title := matrixTitle()
	title.Language = "ja"
	nordic := rel("nordic", "The.Signal.1999.1080p.WEB-DL.NORDIC-GRP", 8000)
	nordic.Info = signalReleaseInfo(1080, nil, nil)
	original := rel("original", "The.Signal.1999.720p.WEB-DL-GRP", 4000)
	original.Info = signalReleaseInfo(720, []string{"japanese"}, []string{"norwegian"})
	other := rel("other", "The.Signal.1999.1080p.WEB-DL.ENG-GRP", 9000)
	other.Info = signalReleaseInfo(1080, []string{"english"}, nil)
	cfg := store.Get()
	cfg.Resolver.MaxCandidates = 5
	svc := New(&fakeIndexer{releases: []core.Release{other, original, nordic}}, fakeHealth{}, openTestDB(t), cfg, Options{})
	t.Cleanup(func() { svc.Close() })
	releases := svc.ConsoleHandler(ConsoleDeps{
		Catalog: fakeCatalog{titles: map[core.RatingKey]*core.Title{title.Key: title}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h := admin.New(admin.Deps{
		Config: store,
		IsOwnerToken: func(_ context.Context, token string) bool {
			return token == harnessOwner
		},
		ViewerDirectory: func(context.Context) ([]admin.ViewerRecord, error) {
			return []admin.ViewerRecord{}, nil
		},
		CatalogHelper: harnessTitles{title: title},
		Extensions:    map[string]http.Handler{"releases": releases},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	line := fmt.Sprintf("HARNESS http://%s/ether/ token %s\n", ln.Addr().String(), harnessOwner)
	fmt.Fprint(os.Stderr, line)
	if err := os.WriteFile("/tmp/vrr-harness.txt", []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(20 * time.Minute):
	case <-t.Context().Done():
	}
}

type harnessTitles struct{ title *core.Title }

func (h harnessTitles) SearchTitles(_ context.Context, q string, _ core.MediaType, _ int) ([]core.Title, error) {
	if h.title == nil || (q != "" && !strings.Contains(strings.ToLower(h.title.Title), strings.ToLower(q))) {
		return nil, nil
	}
	return []core.Title{*h.title}, nil
}

func (h harnessTitles) Facets(context.Context, core.MediaType, string) ([]core.FacetValue, error) {
	return nil, nil
}

func (h harnessTitles) Lookup(context.Context, string, string, string) ([]core.LookupResult, error) {
	return nil, nil
}

func (h harnessTitles) TitleDetail(_ context.Context, key core.RatingKey) (*core.Title, error) {
	if h.title == nil || key != h.title.Key {
		return nil, core.ErrNotFound
	}
	cp := *h.title
	return &cp, nil
}

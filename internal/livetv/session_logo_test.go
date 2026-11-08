package livetv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testLogoFetcher struct {
	url  string
	fail bool
}

func (f *testLogoFetcher) GetUntrustedURL(_ context.Context, url string) ([]byte, string, error) {
	f.url = url
	if f.fail {
		return nil, "", errors.New("failed")
	}
	return []byte("logo"), "image/png", nil
}

func TestOwnerChannelLogo(t *testing.T) {
	s := testService(t, Options{})
	c := testChannel()
	c.Logo = "https://images.example/logo.svg?secret=credential"
	if _, err := s.SaveChannel(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	fetch := &testLogoFetcher{}
	h := s.ownerHandler(fetch)
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		return r
	}
	r := get("/channels/test/logo?url=https://attacker.invalid/")
	if r.Code != 200 || r.Body.String() != "logo" || fetch.url != c.Logo || r.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.Header().Get("Content-Security-Policy"), "sandbox") || r.Header().Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("response %d %q %v, fetched %q", r.Code, r.Body, r.Header(), fetch.url)
	}
	if r := get("/channels/missing/logo"); r.Code != 404 {
		t.Fatalf("missing: %d", r.Code)
	}
	fetch.fail = true
	if r := get("/channels/test/logo"); r.Code != 404 || strings.Contains(r.Body.String(), "credential") {
		t.Fatalf("failure: %d %s", r.Code, r.Body)
	}
}

func TestOwnerChannelLogoBlocksPrivateAddress(t *testing.T) {
	s := testService(t, Options{})
	c := testChannel()
	c.Logo = "http://127.0.0.1:1/logo.png"
	if _, err := s.SaveChannel(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/channels/test/logo", nil))
	if r.Code != 404 {
		t.Fatalf("private address: %d", r.Code)
	}
}

type logoFetchResult struct {
	data []byte
	ct   string
	err  error
}

type urlLogoFetcher struct {
	urls map[string]logoFetchResult
}

func (f *urlLogoFetcher) GetUntrustedURL(_ context.Context, url string) ([]byte, string, error) {
	if res, ok := f.urls[url]; ok {
		return res.data, res.ct, res.err
	}
	return nil, "", errors.New("unknown url")
}

func TestOwnerChannelLogoFallsBackToGuideLogo(t *testing.T) {
	s := testService(t, Options{})
	fetcher := &urlLogoFetcher{urls: make(map[string]logoFetchResult)}
	h := s.ownerHandler(fetcher)
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		return r
	}

	// 1. Channel has c.Logo pointing to a URL that fails, but channel has c.GuideID
	// pointing to an entry in livetv_guide_channels with a valid logo URL:
	// handler falls back and serves 200 with guide logo.
	t.Run("failing logo falls back to guide logo", func(t *testing.T) {
		c := testChannel()
		c.ID = "ch-fallback"
		c.SourceID = "src-1"
		c.GuideID = "guide-1"
		c.Logo = "https://example.com/bad-channel-logo.png"
		if _, err := s.SaveChannel(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if _, err := s.o.DB.Exec("INSERT INTO livetv_guide_channels(source_id, guide_id, name, logo) VALUES (?, ?, ?, ?)",
			"src-1", "guide-1", "Channel 1", "https://example.com/good-guide-logo.png"); err != nil {
			t.Fatal(err)
		}
		fetcher.urls["https://example.com/bad-channel-logo.png"] = logoFetchResult{err: errors.New("upstream failed")}
		fetcher.urls["https://example.com/good-guide-logo.png"] = logoFetchResult{data: []byte("guide-logo-content"), ct: "image/png"}

		rec := get("/channels/ch-fallback/logo")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if rec.Body.String() != "guide-logo-content" {
			t.Fatalf("unexpected body %q", rec.Body.String())
		}
	})

	// 2. Channel has empty c.Logo, but has c.GuideID with a guide logo:
	// handler serves 200 with guide logo.
	t.Run("empty logo serves guide logo", func(t *testing.T) {
		c := testChannel()
		c.ID = "ch-empty-logo"
		c.SourceID = "src-1"
		c.GuideID = "guide-2"
		c.Logo = ""
		if _, err := s.SaveChannel(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if _, err := s.o.DB.Exec("INSERT INTO livetv_guide_channels(source_id, guide_id, name, logo) VALUES (?, ?, ?, ?)",
			"src-1", "guide-2", "Channel 2", "https://example.com/guide2-logo.png"); err != nil {
			t.Fatal(err)
		}
		fetcher.urls["https://example.com/guide2-logo.png"] = logoFetchResult{data: []byte("guide-2-logo-content"), ct: "image/png"}

		rec := get("/channels/ch-empty-logo/logo")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if rec.Body.String() != "guide-2-logo-content" {
			t.Fatalf("unexpected body %q", rec.Body.String())
		}
	})

	// 3. Channel has failing c.Logo and failing guide logo: handler returns 404.
	t.Run("failing logo and failing guide logo returns 404", func(t *testing.T) {
		c := testChannel()
		c.ID = "ch-both-fail"
		c.SourceID = "src-1"
		c.GuideID = "guide-3"
		c.Logo = "https://example.com/bad-logo-3.png"
		if _, err := s.SaveChannel(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if _, err := s.o.DB.Exec("INSERT INTO livetv_guide_channels(source_id, guide_id, name, logo) VALUES (?, ?, ?, ?)",
			"src-1", "guide-3", "Channel 3", "https://example.com/bad-guide-logo-3.png"); err != nil {
			t.Fatal(err)
		}
		fetcher.urls["https://example.com/bad-logo-3.png"] = logoFetchResult{err: errors.New("upstream failed")}
		fetcher.urls["https://example.com/bad-guide-logo-3.png"] = logoFetchResult{err: errors.New("guide upstream failed")}

		rec := get("/channels/ch-both-fail/logo")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	// 4. Channel has failing c.Logo and no GuideID: handler returns 404.
	t.Run("failing logo and no guide id returns 404", func(t *testing.T) {
		c := testChannel()
		c.ID = "ch-no-guide"
		c.SourceID = "src-1"
		c.GuideID = ""
		c.Logo = "https://example.com/bad-logo-4.png"
		if _, err := s.SaveChannel(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		fetcher.urls["https://example.com/bad-logo-4.png"] = logoFetchResult{err: errors.New("upstream failed")}

		rec := get("/channels/ch-no-guide/logo")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})

	// 5. Channel has distinct GuideSourceID from SourceID (matched external guide):
	// handler looks up guide logo in GuideSourceID and serves 200.
	t.Run("external guide source logo lookup", func(t *testing.T) {
		c := testChannel()
		c.ID = "ch-external-guide"
		c.SourceID = "m3u-source"
		c.GuideSourceID = "directv-source"
		c.GuideID = "guide-ext-5"
		c.Logo = "http://127.0.0.1:8080/tuliprox.png"
		if _, err := s.SaveChannel(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if _, err := s.o.DB.Exec("INSERT INTO livetv_guide_channels(source_id, guide_id, name, logo) VALUES (?, ?, ?, ?)",
			"directv-source", "guide-ext-5", "Channel 5", "https://example.com/directv-logo.png"); err != nil {
			t.Fatal(err)
		}
		fetcher.urls["http://127.0.0.1:8080/tuliprox.png"] = logoFetchResult{err: errors.New("blocked")}
		fetcher.urls["https://example.com/directv-logo.png"] = logoFetchResult{data: []byte("directv-logo-content"), ct: "image/png"}

		rec := get("/channels/ch-external-guide/logo")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if rec.Body.String() != "directv-logo-content" {
			t.Fatalf("unexpected body %q", rec.Body.String())
		}
	})
}

package plex

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestStartHTTP(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mkv"}
	rec := f.get(t, "/video/:/transcode/universal/start?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=mac-http&protocol=http&offset=12")
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("ct %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Accept-Ranges") != "none" {
		t.Fatalf("accept-ranges %q", rec.Header().Get("Accept-Ranges"))
	}
	if !strings.Contains(rec.Body.String(), "mkv-mac-http") {
		t.Fatalf("body %q", rec.Body.String())
	}
	h := f.trans.sessions["mac-http"]
	if h == nil || h.req.Protocol != core.ProtocolHTTP || h.req.StartOffset != 12*time.Second {
		t.Fatalf("http req: %+v", h)
	}

	head := f.request(t, http.MethodHead, "/video/:/transcode/universal/start?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=mac-http&protocol=http&offset=12", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("head: %d len=%d", head.Code, head.Body.Len())
	}
	if len(f.trans.stopped) != 0 {
		t.Fatalf("restarted without seek: %v", f.trans.stopped)
	}

	rec = f.get(t, "/video/:/transcode/universal/start?path=%2Flibrary%2Fmetadata%2F1000000000000550&session=mac-http&offset=60")
	if rec.Code != http.StatusOK {
		t.Fatalf("seek: %d", rec.Code)
	}
	if len(f.trans.stopped) != 1 || f.trans.sessions["mac-http"].req.StartOffset != 60*time.Second {
		t.Fatalf("seek restart: %v offset=%v", f.trans.stopped, f.trans.sessions["mac-http"].req.StartOffset)
	}

	f.get(t, "/video/:/transcode/universal/ping?session=mac-http")
	f.get(t, "/video/:/transcode/universal/stop?session=mac-http")
	if _, ok := f.trans.sessions["mac-http"]; ok {
		t.Fatal("not stopped")
	}
}

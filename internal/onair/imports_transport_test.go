package onair

import (
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
)

// The import client installs its own guarded dialer; it must not install it
// on the transport egress shares with every other plugin client.
func TestImportClientDoesNotMutateSharedPluginTransport(t *testing.T) {
	shared := egress.Client(egress.CapPlugins, time.Minute).Transport.(*http.Transport)
	a, b := importClient(), importClient()
	if a.Transport == http.RoundTripper(shared) || b.Transport == http.RoundTripper(shared) || a.Transport == b.Transport {
		t.Fatal("import client reuses a shared transport")
	}
	if egress.Client(egress.CapPlugins, time.Minute).Transport.(*http.Transport) != shared {
		t.Fatal("expected egress to keep sharing its plugin transport")
	}
}

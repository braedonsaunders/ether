package mediabrowser

import (
	"context"
	"testing"
)

func TestStatusIsUnverifiedWithoutEmbeddedEvidence(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	i.info.Version = "12.4.0"
	status := i.Status(context.Background())
	// Embedded evidence matches 12.4.0 with a clean corpus and no certified
	// client, which is partial rather than unverified.
	if status.TargetVersion != "12.4.0" || status.Conformance != "partial" {
		t.Fatal(status)
	}
}

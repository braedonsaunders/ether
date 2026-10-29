package mediabrowser

import (
	"context"
	"testing"
)

func TestStatusIsUnverifiedWithoutEmbeddedEvidence(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	i.info.Version = "12.4.0"
	status := i.Status(context.Background())
	if status.TargetVersion != "12.4.0" || status.Conformance != "unverified" {
		t.Fatal(status)
	}
}

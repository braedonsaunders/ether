package streamgrant

import (
	"strings"
	"testing"
)

func TestGrantURLStripsAccountCredentials(t *testing.T) {
	got := GrantURL("/live/index.m3u8?X-Plex-Token=account-secret&api_key=other-secret&keep=1", "stream-grant")
	if strings.Contains(got, "account-secret") || strings.Contains(got, "other-secret") || strings.Contains(got, "X-Plex-Token") || strings.Contains(got, "api_key") {
		t.Fatalf("credential leaked in %s", got)
	}
	if !strings.Contains(got, StreamGrantParam+"=stream-grant") || !strings.Contains(got, "keep=1") {
		t.Fatalf("grant url = %s", got)
	}
}

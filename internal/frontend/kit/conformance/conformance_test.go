package conformance

import "testing"

func TestStatusRequiresMatchingEvidence(t *testing.T) {
	cases := []struct{ raw, version, want string }{
		{`{"schema":1,"protocols":{}}`, "12.4", "unverified"},
		{`{"schema":1,"protocols":{"jellyfin":{"protocolVersion":"12.3","date":"2026-01-01T00:00:00Z","blocking":0,"certifiedClients":["client"]}}}`, "12.4", "unverified"},
		{`{"schema":1,"protocols":{"jellyfin":{"protocolVersion":"12.4","date":"2026-01-01T00:00:00Z","blocking":3,"certifiedClients":[]}}}`, "12.4", "failing"},
		{`{"schema":1,"protocols":{"jellyfin":{"protocolVersion":"12.4","date":"2026-01-01T00:00:00Z","blocking":0,"certifiedClients":[]}}}`, "12.4", "partial"},
		{`{"schema":1,"protocols":{"jellyfin":{"protocolVersion":"12.4","date":"2026-01-01T00:00:00Z","blocking":0,"certifiedClients":["client 1.0"]}}}`, "12.4", "passing"},
		{`{"schema":1,"protocols":{"jellyfin":{"protocolVersion":"12.4","date":"bad","blocking":0,"certifiedClients":[]}}}`, "12.4", "unverified"},
		{`{"schema":1,"protocols":{"jellyfin":{"protocolVersion":"12.4","date":"2026-01-01T00:00:00Z","certifiedClients":[]}}}`, "12.4", "unverified"},
	}
	for _, c := range cases {
		if got := StatusFrom([]byte(c.raw), "jellyfin", c.version); got != c.want {
			t.Errorf("%s: %s want %s", c.raw, got, c.want)
		}
	}
	if got := Status("jellyfin", "12.4"); got != "unverified" {
		t.Fatalf("empty embedded evidence: %s", got)
	}
}

package notify

// EventInfo distinguishes automatic producers from owner API integrations.
// Existing event identifiers remain accepted for compatibility.
type EventInfo struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Automatic   bool   `json:"automatic"`
}

func eventCatalog() []EventInfo {
	descriptions := map[string]string{
		"server.started":  "This node is listening and ready for connections.",
		"server.stopping": "A normal shutdown or restart has begun.",
		"server.crash":    "Independent watcher detects an unexpected process exit; sent immediately, outside the queue.",
		"playback.start":  "A viewer starts a stream.", "playback.stop": "A viewer ends a stream.", "playback.error": "Playback fails.", "playback.failover": "A stream switches to another source.",
		"provider.outage": "A provider enters backoff or rejects authentication.", "provider.recovered": "An unavailable provider responds again.",
		"indexer.outage": "An indexer operation fails; one alert per outage.", "indexer.budget": "An indexer reports budget exhaustion.", "indexer.recovered": "An indexer operation succeeds after an alert.",
		"cache.full": "The segment cache reaches its configured budget.", "cache.recovered": "The cache drops below its budget again.",
		"title.available": "A monitored watchlist title becomes available.", "newsletter.weekly": "Scheduled viewing summary; email channels only.",
		"update.available": "A new Ether release is published; one alert per version.",
		"backup.failed":    "A scheduled backup fails; one alert until a backup succeeds again.",
	}
	out := make([]EventInfo, 0, len(Kinds))
	for _, k := range Kinds {
		if k == "test" {
			continue
		}
		d, automatic := descriptions[k]
		if !automatic {
			d = "Owner API integration only; Ether does not automatically produce this event."
		}
		out = append(out, EventInfo{Kind: k, Description: d, Automatic: automatic})
	}
	return out
}
func (s *Service) monitorActive() bool {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if s.watchUpdate == nil {
		return false
	}
	select {
	case <-s.monitorDone:
		return false
	default:
		return true
	}
}

package config

// upgradeShippedQuality moves an install still on the quality defaults
// releases up to v0.3.1 shipped (1080p at home and away, an Apple TV rule
// for 4K, an 8 Mbps remote cap) to the current ones: no server cap at
// home or away, each device held to what it can play. Anything the owner
// changed is left alone. It reports whether cfg changed.
func upgradeShippedQuality(cfg *Config) bool {
	q := &cfg.Quality
	if q.Default != "1080p" || q.Remote != "1080p" || len(q.Rules) != 1 || q.Rules[0] != (QualityRule{Product: "Apple TV", Profile: "4K"}) {
		return false
	}
	uhd := -1
	for i, p := range q.Profiles {
		if p.Name == "4K" {
			uhd = i
		}
	}
	if uhd < 0 {
		return false
	}
	q.Default, q.Remote, q.Rules = "4K", "", nil
	if q.Profiles[uhd].MinHeight == 1080 {
		q.Profiles[uhd].MinHeight = 720
	}
	if cfg.Transcode.RemoteMaxBitrateKbps == 8000 {
		cfg.Transcode.RemoteMaxBitrateKbps = 0
	}
	return true
}

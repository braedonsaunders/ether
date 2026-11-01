package admin

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestValidateConfigQualityProfileRanking(t *testing.T) {
	cfg := config.Defaults(t.TempDir())
	if errs := ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("Defaults failed ValidateConfig: %v", errs)
	}

	// Custom valid ranking with a Prefer rule.
	cfg.Quality.Profiles[0].ReleaseRules = []core.ReleaseRule{
		{
			ID:     "r1",
			Name:   "Prefer FLUX",
			Action: "prefer",
			Conditions: []core.ReleaseCondition{
				{Field: "group", Operator: "equals", Value: "FLUX"},
			},
		},
	}
	cfg.Quality.Profiles[0].Ranking = []string{
		core.RankLanguage,
		core.RankConnection,
		core.RankReleaseType,
		core.RankResolution,
		core.RankRulePrefix + "r1",
		core.RankHDR,
		core.RankVerified,
		core.RankVideoCodec,
		core.RankAudioFormat,
		core.RankSize,
	}
	if errs := ValidateConfig(cfg); len(errs) != 0 {
		t.Fatalf("valid custom ranking failed ValidateConfig: %v", errs)
	}

	// Invalid ranking missing "connection".
	cfg.Quality.Profiles[0].Ranking = []string{
		core.RankLanguage,
		core.RankResolution,
		core.RankVerified,
	}
	errs := ValidateConfig(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "quality.profiles[0].ranking" && strings.Contains(e.Message, "connection") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected quality.profiles[0].ranking error for missing connection, got %v", errs)
	}
}

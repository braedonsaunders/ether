package resolver

import (
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestProtocolRuleMatching(t *testing.T) {
	tests := []struct {
		rule config.QualityRule
		caps core.ClientCaps
		want bool
	}{
		{config.QualityRule{Protocol: "jellyfin"}, core.ClientCaps{Protocol: "jellyfin"}, true},
		{config.QualityRule{Protocol: "jellyfin"}, core.ClientCaps{Protocol: "plex"}, false},
		{config.QualityRule{Protocol: "PLEX", Product: "tv"}, core.ClientCaps{Protocol: "plex", Product: "Plex TV"}, true},
		{config.QualityRule{Product: "tv"}, core.ClientCaps{Protocol: "silo", Product: "Silo TV"}, true},
		{config.QualityRule{}, core.ClientCaps{Protocol: "plex"}, false},
	}
	for _, v := range tests {
		if got := ruleMatches(v.rule, v.caps, nil); got != v.want {
			t.Fatalf("%+v %+v: %v", v.rule, v.caps, got)
		}
	}
}

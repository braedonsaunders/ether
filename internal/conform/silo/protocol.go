// Package silo provides Silo's native profile-driven conformance protocol.
package silo

import (
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
)

// Protocol uses the shared profile transport while keeping Silo's native auth
// and discovery behavior separate from the Jellyfin and Emby dialects.
type Protocol struct{ *mediabrowser.Protocol }

var _ conform.Protocol = (*Protocol)(nil)

func Load(dir string) (*Protocol, error) {
	p, err := mediabrowser.Load("silo", dir)
	if err != nil {
		return nil, err
	}
	return &Protocol{p}, nil
}

// Observe carries only a playable session into subsequent ordered flow probes.
func (*Protocol) Observe(request conform.Request, response conform.Response, vars map[string]string) {
	if request.Path != "/api/v2/playback/start" || response.Status < 200 || response.Status >= 300 || vars == nil {
		return
	}
	var result struct {
		Outcome   string `json:"outcome"`
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(response.Body, &result) == nil && result.Outcome == "playable" && result.SessionID != "" {
		vars["play_session_id"] = result.SessionID
		vars["session_id"] = result.SessionID
	}
}

var _ conform.ResponseObserver = (*Protocol)(nil)

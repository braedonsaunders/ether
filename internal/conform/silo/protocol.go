// Package silo provides Silo's native profile-driven conformance protocol.
package silo

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
)

type Protocol struct{ *mediabrowser.Protocol }

var _ conform.Protocol = (*Protocol)(nil)

func Load(dir string) (*Protocol, error) {
	p, err := mediabrowser.Load("silo", dir)
	if err != nil {
		return nil, err
	}
	return &Protocol{p}, nil
}

// Observe carries a playable session and its relative HLS paths into the
// subsequent ordered probes. It never accepts an upstream-supplied hostname.
func (*Protocol) Observe(request conform.Request, response conform.Response, vars map[string]string) {
	if response.Status < 200 || response.Status >= 300 || vars == nil {
		return
	}
	if request.Path == "/api/v2/playback/start" {
		var result struct {
			Outcome      string `json:"outcome"`
			SessionID    string `json:"session_id"`
			PlaybackPlan struct {
				SessionID string `json:"session_id"`
				Stream    struct {
					URL string `json:"url"`
				} `json:"stream"`
			} `json:"playback_plan"`
		}
		if json.Unmarshal(response.Body, &result) != nil || result.Outcome != "playable" {
			return
		}
		id := result.SessionID
		if id == "" {
			id = result.PlaybackPlan.SessionID
		}
		if id != "" {
			vars["play_session_id"] = id
			vars["session_id"] = id
		}
		if path := relativePath(result.PlaybackPlan.Stream.URL); path != "" {
			vars["stream_url"] = path
			rememberToken(path, vars)
		}
		return
	}
	if request.Path != "${stream_url}" {
		return
	}
	base, err := url.Parse(vars["stream_url"])
	if err != nil {
		return
	}
	defs := map[string]string{}
	for _, line := range strings.Split(string(response.Body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-DEFINE:") {
			attrs := map[string]string{}
			for _, m := range defineAttr.FindAllStringSubmatch(strings.TrimPrefix(line, "#EXT-X-DEFINE:"), -1) {
				attrs[m[1]] = m[2]
			}
			if validVariable.MatchString(attrs["NAME"]) && attrs["VALUE"] != "" {
				defs[attrs["NAME"]] = attrs["VALUE"]
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for name, value := range defs {
			line = strings.ReplaceAll(line, "{$"+name+"}", value)
		}
		if strings.ContainsAny(line, "{}") {
			return
		}
		ref, err := url.Parse(line)
		if err != nil || ref.IsAbs() || ref.Host != "" {
			return
		}
		resolved := base.ResolveReference(ref)
		if path := relativePath(resolved.String()); path != "" {
			vars["segment_url"] = path
			rememberToken(path, vars)
		}
		return
	}
}
func relativePath(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return ""
	}
	return u.RequestURI()
}
func rememberToken(path string, vars map[string]string) {
	u, err := url.Parse(path)
	if err == nil {
		if token := u.Query().Get("st"); token != "" {
			vars["stream_token"] = token
		}
	}
}

var _ conform.ResponseObserver = (*Protocol)(nil)

var defineAttr = regexp.MustCompile(`(NAME|VALUE)="([^"\r\n]*)"`)
var validVariable = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

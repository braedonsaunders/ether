// Package silo provides Silo's native profile-driven conformance protocol.
package silo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// Compare treats valid Silo HLS implementation choices as informational while
// retaining structural and semantic failures as blocking.
func (p *Protocol) Compare(reference, target conform.Response) conform.Report {
	if bytes.HasPrefix(bytes.TrimSpace(reference.Body), []byte("#EXTM3U")) || bytes.HasPrefix(bytes.TrimSpace(target.Body), []byte("#EXTM3U")) {
		return compareSiloPlaylist(reference, target)
	}
	return p.Protocol.Compare(reference, target)
}

// Corpus runs the Apple capture's playback before the catalog item read.
// That item response includes last-play facts created by the preceding start.
func (p *Protocol) Corpus(profile string) []conform.Request {
	requests := p.Protocol.Corpus(profile)
	if profile != "silo-apple" {
		return requests
	}
	var earlier, playback, item []conform.Request
	for _, request := range requests {
		switch {
		case strings.HasPrefix(request.Name, "item_detail_"):
			item = append(item, request)
		case strings.HasPrefix(request.Name, "play_"), strings.HasPrefix(request.Name, "progress_"), strings.HasPrefix(request.Name, "mark_"), strings.HasPrefix(request.Name, "stop_"):
			playback = append(playback, request)
		default:
			earlier = append(earlier, request)
		}
	}
	return append(append(earlier, playback...), item...)
}

func (p *Protocol) CompareRequest(request conform.Request, reference, target conform.Response) conform.Report {
	if bytes.HasPrefix(bytes.TrimSpace(reference.Body), []byte("#EXTM3U")) || bytes.HasPrefix(bytes.TrimSpace(target.Body), []byte("#EXTM3U")) {
		return compareSiloPlaylist(reference, target)
	}
	return p.Protocol.CompareRequest(request, reference, target)
}

// Fetch follows the first HLS variant when a target exposes a master ladder.
// The corpus records a media playlist, so the subsequent segment probes must
// resolve against the selected variant rather than treating it as a segment.
func (p *Protocol) Fetch(ctx context.Context, client *http.Client, base, token string, request conform.Request, profile string, vars map[string]string, jsonMode bool) (conform.Response, error) {
	response, err := p.Protocol.Fetch(ctx, client, base, token, request, profile, vars, jsonMode)
	if err != nil || request.Path != "${stream_url}" || response.Status != http.StatusOK {
		return response, err
	}
	seen := map[string]bool{}
	for depth := 0; depth < 4; depth++ {
		path, master, e := masterVariant(vars["stream_url"], response.Body)
		if e != nil {
			return conform.Response{}, e
		}
		if !master {
			return response, nil
		}
		if seen[path] {
			return conform.Response{}, fmt.Errorf("HLS playlist cycle")
		}
		seen[path] = true
		vars["stream_url"] = path
		next := request
		next.Path = path
		response, err = p.Protocol.Fetch(ctx, client, base, token, next, profile, vars, jsonMode)
		if err != nil || response.Status != http.StatusOK {
			return response, err
		}
	}
	return conform.Response{}, fmt.Errorf("HLS master nesting limit exceeded")
}

func masterVariant(basePath string, body []byte) (string, bool, error) {
	base, err := url.Parse(basePath)
	if err != nil || base.IsAbs() || base.Host != "" {
		return "", false, fmt.Errorf("invalid HLS playlist path")
	}
	want := false
	definitions := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-DEFINE:") {
			attrs := map[string]string{}
			for _, match := range defineAttr.FindAllStringSubmatch(strings.TrimPrefix(line, "#EXT-X-DEFINE:"), -1) {
				attrs[match[1]] = match[2]
			}
			if validVariable.MatchString(attrs["NAME"]) && attrs["VALUE"] != "" {
				definitions[attrs["NAME"]] = attrs["VALUE"]
			}
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			want = true
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !want {
			continue
		}
		for name, value := range definitions {
			line = strings.ReplaceAll(line, "{$"+name+"}", value)
		}
		ref, e := url.Parse(line)
		if e != nil || ref.IsAbs() || ref.Host != "" || strings.ContainsAny(line, "{}") {
			return "", true, fmt.Errorf("invalid HLS variant")
		}
		path := relativePath(base.ResolveReference(ref).String())
		if path == "" {
			return "", true, fmt.Errorf("invalid HLS variant")
		}
		return path, true, nil
	}
	if want {
		return "", true, fmt.Errorf("HLS master has no variant")
	}
	return "", false, nil
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

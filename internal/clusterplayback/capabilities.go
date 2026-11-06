package clusterplayback

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/nodewire/outbound"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"io"
	"net/http"
	"time"
)

const CapabilitiesPath = "/cluster/playback/capabilities"

type capabilityReply struct {
	Identity     nodewire.Identity
	Capabilities remotetranscode.Capabilities
	Supported    bool
}

// No process or provider work is allowed in this trusted backend preflight.
type requestSupport interface{ SupportsDistributedRequest(core.HLSRequest) bool }

func (r *Runtime) supportsBackend(q core.HLSRequest) bool {
	if r.o.Encoder == nil || !remotetranscode.SupportsRequest(q, r.o.WorkerCapabilities) {
		return false
	}
	backend, ok := r.o.Encoder.(requestSupport)
	return ok && backend.SupportsDistributedRequest(q)
}
func (r *Runtime) capabilitiesHTTP(w http.ResponseWriter, q *http.Request) {
	peer, e := r.o.Guard.Request(q)
	if e != nil || r.o.MemberRole(q.Context(), peer, config.RoleServer) != nil {
		http.Error(w, "denied", 401)
		return
	}
	if q.Method != "POST" {
		http.Error(w, "method", 405)
		return
	}
	var wire capabilityRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, q.Body, remotetranscode.MaxMessage))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "unsupported", 422)
		return
	}
	request := wire.Request
	request.Info.Keyframes = wire.Keyframes
	if !validPreparedKeyframes(request.Info) || request.InputURL != "" || !remotetranscode.SupportsRequest(request, r.o.WorkerCapabilities) {
		http.Error(w, "unsupported", 422)
		return
	}
	supported := r.o.Eligible(q.Context(), r.o.Self, config.RoleTranscode) == nil && r.supportsBackend(request)
	reply := capabilityReply{Identity: r.o.Self, Capabilities: r.o.WorkerCapabilities, Supported: supported}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}
func (r *Runtime) workerCapabilities(ctx context.Context, id nodewire.Identity, q core.HLSRequest) (remotetranscode.Capabilities, error) {
	q.InputURL = ""
	ep, tc, check, e := r.peer(ctx, id, CapabilitiesPath)
	if e != nil {
		return remotetranscode.Capabilities{}, e
	}
	transport, e := outbound.New(ep, tc, 5*time.Second, check)
	if e != nil {
		return remotetranscode.Capabilities{}, e
	}
	defer transport.CloseIdleConnections()
	raw, e := json.Marshal(capabilityRequest{Request: q, Keyframes: q.Info.Keyframes})
	if e != nil || len(raw) > remotetranscode.MaxMessage {
		return remotetranscode.Capabilities{}, ErrDenied
	}
	request, e := http.NewRequestWithContext(ctx, "POST", ep, bytes.NewReader(raw))
	if e != nil {
		return remotetranscode.Capabilities{}, e
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Do(request)
	if e != nil {
		return remotetranscode.Capabilities{}, e
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnprocessableEntity {
		return remotetranscode.Capabilities{}, remotetranscode.ErrUnsupported
	}
	if response.StatusCode != 200 {
		return remotetranscode.Capabilities{}, ErrDenied
	}
	raw, e = io.ReadAll(io.LimitReader(response.Body, remotetranscode.MaxMessage+1))
	if e != nil || len(raw) > remotetranscode.MaxMessage {
		return remotetranscode.Capabilities{}, ErrDenied
	}
	var reply capabilityReply
	if json.Unmarshal(raw, &reply) != nil || reply.Identity != id {
		return remotetranscode.Capabilities{}, ErrDenied
	}
	caps, e := remotetranscode.NormalizeCapabilities(reply.Capabilities)
	if e != nil {
		return remotetranscode.Capabilities{}, ErrDenied
	}
	if !reply.Supported || !remotetranscode.SupportsRequest(q, caps) {
		return remotetranscode.Capabilities{}, remotetranscode.ErrUnsupported
	}
	return intersectCapabilities(caps, r.o.RemoteCapabilities), nil
}
func intersectCapabilities(a, b remotetranscode.Capabilities) remotetranscode.Capabilities {
	return remotetranscode.Capabilities{HLS: a.HLS && b.HLS, SegmentSeek: a.SegmentSeek && b.SegmentSeek, DASH: a.DASH && b.DASH, HTTP: a.HTTP && b.HTTP, ABR: a.ABR && b.ABR, SoftSubtitles: a.SoftSubtitles && b.SoftSubtitles, Controls: a.Controls && b.Controls, Keyframes: a.Keyframes && b.Keyframes, Observations: a.Observations && b.Observations, RecoveryStart: a.RecoveryStart && b.RecoveryStart}
}

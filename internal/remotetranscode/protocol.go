// Package remotetranscode provides an opt-in private HLS execution foundation.
// It installs no routes and changes no local transcoder behavior.
package remotetranscode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

const MaxMessage = 64 << 10
const Version = 1

var ErrUnsupported = errors.New("remotetranscode: unsupported capability")
var ErrUncertain = errors.New("remotetranscode: execution responsibility requires reconciliation")

// Capabilities must be consulted by placement before choosing this adapter.
// Segment seeks use the backend's ordinary ServeSegment behavior. Source swaps,
// probe, subtitles, DASH, HTTP Matroska, ABR and optional session controls are not supported.
type Capabilities struct {
	HLS, SegmentSeek, DASH, HTTP, Probe, SourceSwap, Subtitles, ABR, SoftSubtitles, Controls, Observations, Keyframes bool
	// RecoveryStart negotiates instruction transport only, never successful recovery.
	RecoveryStart bool `json:",omitempty"`
}

// NormalizeCapabilities preserves fixed HLS for zero options and rejects promises
// for operations that have no concrete private implementation.
func NormalizeCapabilities(c Capabilities) (Capabilities, error) {
	if c == (Capabilities{}) {
		return Supported(), nil
	}
	if !c.HLS || !c.SegmentSeek || c.Probe || c.SourceSwap || c.Subtitles {
		return Capabilities{}, ErrUnsupported
	}
	return c, nil
}

func Supported() Capabilities { return Capabilities{HLS: true, SegmentSeek: true} }

// Identity is assigned by trusted placement, never by a public request decoder.
// Controller and Worker are boot-scoped identities, not stable node names alone.
type Identity struct {
	Controller string
	Worker     string
	Session    string
	Generation uint64
}
type Start struct {
	RecoveryStart      *core.RecoveryStart  `json:",omitempty"`
	Keyframes          []core.KeyframePoint `json:",omitempty"`
	SoftSubtitleStream *int                 `json:",omitempty"`
	Protocol           string               `json:",omitempty"`
	ReleaseID          string
	Identity           Identity
	Grant              string
	Expires            time.Time
	Info               core.MediaInfo
	Decision           core.Decision
	Offset             time.Duration
	SegmentDuration    time.Duration
	IndependentProof   bool
}

// Renewal is a compare-and-swap of the execution lease, never a new Start.
// Operation is the next version, scoped to Identity; zero is the initial lease.
// Deadlines on the wire must use UTC. Equality compares canonical instants.
type Renewal struct {
	Identity         Identity
	Operation        uint64
	PreviousVersion  uint64
	PreviousDeadline time.Time
	Deadline         time.Time
}

// RenewalReceipt is returned only by a trusted injected validator, never decoded
// from the caller. It attests durable placement AND continued access using the
// SAME source authority handle, gateway URL and input lease through Deadline.
// Start.Grant stays immutable; internal source bearers may rotate while preserving
// exact source scope/revision/size. The validator must actually
// extend/revalidate that source authority; an expired bearer alone is not proof.
// No source replacement or encoder invocation is permitted in this callback.
// It must account for DB/worker clock skew conservatively and fail on uncertainty.
type RenewalReceipt struct {
	Renewal        Renewal
	PlacementUntil time.Time
	SourceUntil    time.Time
}
type ValidateRenewal func(context.Context, string, Start, Renewal) (RenewalReceipt, error)

// LeaseState distinguishes accepted lease history from current execution state.
// Accepted is the latest operation only; older retries reconcile this snapshot.
type LeaseState struct {
	Deadline  time.Time
	Version   uint64
	Accepted  *Renewal `json:",omitempty"`
	Renewable bool
}

type message struct {
	Version        int
	Head           bool `json:",omitempty"`
	Op             string
	Identity       Identity
	Renewal        *Renewal `json:",omitempty"`
	Start          *Start   `json:",omitempty"`
	Segment        int      `json:",omitempty"`
	Rung           string   `json:",omitempty"`
	Representation int      `json:",omitempty"`
	State          string   `json:",omitempty"`
	Subtitle       int      `json:",omitempty"`
}
type status struct {
	IndependentSegments bool           `json:",omitempty"`
	Rungs               []core.HLSRung `json:",omitempty"`
	Stopped             bool
	Lease               LeaseState
	State               string
	Identity            Identity
}

// Security authenticates mTLS peer identity and authorizes the complete operation.
// Authorize must check current placement/source authority, generation and expiry;
// an authenticated certificate alone does not authorize work. Called for reads too.
// For renew, Start is nil and ValidateRenewal separately validates its full DTO.
// Permit exact scoped stop/status after expiry; neither grants execution authority.
type Security struct {
	Authenticate func(*http.Request) (string, error)
	Authorize    func(context.Context, string, string, Identity, *Start) error
}

// TrustedGatewayInput is a trusted, injected composition boundary, never a wire
// DTO. It must bind the exact execution and immutable grant handle to an existing
// loopback listener capability. Return the acquired lease even on error. Arbitrary
// implementations are not made safe by this interface; host composition owns trust.
type TrustedGatewayInput interface {
	GatewayInput(Identity, string) (string, core.EncoderInputLease, error)
}

// Source is produced by trusted source-grant validation, never by a wire URL.
// Ordinary URL/Lease uses the existing private literal-IP HTTPS contract, with
// redirects/nested fetches forbidden and exact identity/expiry binding. Gateway
// is an exclusive trusted local capability. Interface implementations must bind
// identity, origin/path, and lease, and must never redirect or substitute input.
type Source struct {
	// Gateway excludes URL/Lease. Resolver must never acquire an unused second lease.
	Gateway TrustedGatewayInput
	URL     string
	Lease   core.EncoderInputLease
}
type ResolveSource func(context.Context, string, Start) (Source, error)

func validIdentity(i Identity) bool {
	return i.Generation > 0 && token(i.Controller) && token(i.Worker) && token(i.Session)
}
func token(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func privateURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return false
	}
	ip, e := netip.ParseAddr(u.Hostname())
	return e == nil && ip.IsPrivate() && !ip.IsLoopback() && !ip.Is4In6() && !strings.Contains(u.Host, "%")
}
func gatewayURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" || u.RawPath != "" || u.ForceQuery || u.Path == "" || u.Port() == "" {
		return false
	}
	ip, e := netip.ParseAddr(u.Hostname())
	return e == nil && ip.IsLoopback() && !ip.Is4In6() && !strings.Contains(u.Host, "%")
}
func supported(r core.HLSRequest) bool { return SupportsRequest(r, Supported()) }

// SupportsRequest is side-effect-free and shared with placement preflight.
func SupportsRequest(r core.HLSRequest, c Capabilities) bool {
	var err error
	c, err = NormalizeCapabilities(c)
	if err != nil {
		return false
	}
	if r.RecoveryStart != nil && (!c.RecoveryStart || !transcode.ValidRecoveryStartShape(r)) {
		return false
	}
	if (r.Protocol == core.ProtocolDASH && !c.DASH) || (r.Protocol == core.ProtocolHTTP && !c.HTTP) || (r.Protocol != "" && r.Protocol != core.ProtocolHLS && r.Protocol != core.ProtocolDASH && r.Protocol != core.ProtocolHTTP) {
		return false
	}
	if r.ManifestSessionID != "" || len(r.ReleaseID) > 512 {
		return false
	}
	if r.InputLease != nil || len(r.InputHeaders) > 0 || r.LiveInput || r.CopyTimestamps || len(r.SlateText) > 0 {
		return false
	}
	if !ValidateKeyframes(r.Info) || (len(r.Info.Keyframes) > 0 && !c.Keyframes) {
		return false
	}
	if r.SoftSubtitleStream != nil {
		if !c.SoftSubtitles || r.Protocol == core.ProtocolHTTP || !softSubtitle(r.Info, *r.SoftSubtitleStream) {
			return false
		}
	}
	b, err := json.Marshal(Start{RecoveryStart: core.CloneRecoveryStart(r.RecoveryStart), Keyframes: r.Info.Keyframes, ReleaseID: r.ReleaseID, Info: r.Info, Decision: r.Decision, Offset: r.StartOffset, SegmentDuration: r.SegmentDuration, IndependentProof: r.RequireIndependentProof})
	if err != nil || len(b) > MaxMessage-8192 {
		return false
	}
	decision := r.Decision
	if decision.SubtitleStream >= 0 {
		if r.SoftSubtitleStream == nil || *r.SoftSubtitleStream != decision.SubtitleStream {
			return false
		}
		decision.SubtitleStream = -1 // Selection is validated above; retain original DTO on wire/backend.
	}
	return validPlan(r.Info, decision, r.StartOffset, r.SegmentDuration)
}
func validPlan(info core.MediaInfo, d core.Decision, offset, segment time.Duration) bool {
	if len(info.Streams) > 64 || len(info.Chapters) > 128 || offset < 0 || segment < 0 || segment > time.Minute || info.Duration < 0 {
		return false
	}
	if d.Kind != core.DecisionRemux && d.Kind != core.DecisionTranscode {
		return false
	}
	if d.BurnSubtitles || d.SubtitleStream >= 0 || len(d.AudioFilters) > 0 || !transcode.SupportsDistributedEnhancement(d.Enhancement) {
		return false
	}
	for _, s := range info.Streams {
		if s.External || s.ExternalURL != "" {
			return false
		}
	}
	return true
}

func sameRenewal(a, b Renewal) bool {
	return a.Identity == b.Identity && a.Operation == b.Operation && a.PreviousVersion == b.PreviousVersion && a.PreviousDeadline.Equal(b.PreviousDeadline) && a.Deadline.Equal(b.Deadline)
}
func validRenewal(r Renewal) bool {
	_, oldOffset := r.PreviousDeadline.Zone()
	_, newOffset := r.Deadline.Zone()
	return validIdentity(r.Identity) && r.Operation != 0 && r.PreviousVersion != ^uint64(0) && r.Operation == r.PreviousVersion+1 && !r.PreviousDeadline.IsZero() && oldOffset == 0 && newOffset == 0 && r.Deadline.After(r.PreviousDeadline)
}

const MaxKeyframes = transcode.MaxDistributedKeyframes

// ValidateKeyframes is shared by runtime capability/source DTOs and worker
// admission. It validates the complete index without changing any point.
func ValidateKeyframes(info core.MediaInfo) bool { return transcode.ValidateDistributedKeyframes(info) }
func startInfo(s Start) core.MediaInfo {
	info := core.CloneMediaInfo(s.Info)
	info.Keyframes = append([]core.KeyframePoint(nil), s.Keyframes...)
	return info
}

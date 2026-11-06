// Package nodecontrol transports a small owner-authorized allowlist through
// shared storage. Bus messages are hints; only the exact target writes receipts.
package nodecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/clusterconfig"
)

const Topic = "nodecontrol.changed"
const Freshness = 30 * time.Second

type Patch struct {
	Weight   *int  `json:"weight,omitempty"`
	Draining *bool `json:"draining,omitempty"`
}
type Request struct {
	RequestID       string `json:"requestId"`
	Boot            string `json:"boot"`
	ExpectedVersion int64  `json:"expectedVersion"`
	Patch           Patch  `json:"patch"`
}
type Capability struct {
	Key       string `json:"key"`
	Scope     string `json:"scope"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}
type Observed struct {
	Weight   int  `json:"weight"`
	Draining bool `json:"draining"`
}
type Control struct {
	NodeID           string       `json:"nodeId"`
	Boot             string       `json:"boot"`
	RequestedVersion int64        `json:"requestedVersion"`
	AppliedVersion   int64        `json:"appliedVersion"`
	State            string       `json:"state"`
	RequestID        string       `json:"requestId,omitempty"`
	ErrorCode        string       `json:"errorCode,omitempty"`
	UpdatedAt        time.Time    `json:"updatedAt"`
	Observed         Observed     `json:"observed"`
	Capabilities     []Capability `json:"capabilities"`
}
type document struct {
	Control
	Patch Patch `json:"patch"`
}
type Target interface {
	Observe() (Observed, []Capability)
	Apply(context.Context, Patch) string
}
type Service struct {
	Store        *clusterconfig.NodeControls
	Bus          clusterbus.Bus
	NodeID, Boot string
	Target       Target
}

var ErrUnsupported = errors.New("unsupported setting")
var ErrInvalid = errors.New("invalid request")

func (s *Service) load(ctx context.Context, node, boot string) (document, int64, error) {
	r, err := s.Store.Load(ctx, node, boot)
	if err != nil {
		return document{}, 0, err
	}
	var d document
	err = json.Unmarshal(r.Document, &d)
	return d, r.Version, err
}
func (s *Service) Get(ctx context.Context, node, boot string) (Control, error) {
	d, _, err := s.load(ctx, node, boot)
	return d.Control, err
}
func (s *Service) Submit(ctx context.Context, node string, r Request) (Control, error) {
	if node == "" || len(node) > 128 || r.Boot == "" || len(r.Boot) > 128 || r.RequestID == "" || len(r.RequestID) > 128 || r.ExpectedVersion < 0 || (r.Patch.Weight == nil && r.Patch.Draining == nil) || (r.Patch.Weight != nil && (*r.Patch.Weight < 1 || *r.Patch.Weight > 100)) {
		return Control{}, ErrInvalid
	}
	payload, _ := json.Marshal(r)
	d, v, err := s.load(ctx, node, r.Boot)
	if err != nil {
		return Control{}, err
	}
	match, err := s.Store.RequestMatches(ctx, node, r.Boot, r.RequestID, payload)
	if err != nil {
		return Control{}, err
	}
	if match {
		if d.RequestID != r.RequestID {
			return Control{}, clusterconfig.ErrNodeControlConflict
		}
		return d.Control, nil
	}
	if d.State == "pending" || d.RequestedVersion != r.ExpectedVersion || time.Since(d.UpdatedAt) > Freshness || time.Until(d.UpdatedAt) > 5*time.Second {
		return Control{}, clusterconfig.ErrNodeControlConflict
	}
	for key, present := range map[string]bool{"weight": r.Patch.Weight != nil, "draining": r.Patch.Draining != nil} {
		if !present {
			continue
		}
		supported := false
		for _, c := range d.Capabilities {
			if c.Key == key && c.Supported {
				supported = true
			}
		}
		if !supported {
			return Control{}, ErrUnsupported
		}
	}
	d.RequestedVersion++
	d.RequestID = r.RequestID
	d.State = "pending"
	d.ErrorCode = ""
	d.Patch = r.Patch
	raw, _ := json.Marshal(d)
	_, err = s.Store.Submit(ctx, node, r.Boot, r.RequestID, payload, raw, v)
	if err != nil {
		return Control{}, err
	}
	// Publication is never an acknowledgement, even when delivery is synchronous.
	if s.Bus != nil {
		_ = s.Bus.Publish(ctx, Topic, nil)
	}
	return d.Control, nil
}

// Run owns no detached work: callbacks only signal, and shutdown unsubscribes.
// Periodic reconciliation catches missed bus notifications. A new boot never
// consumes the previous boot's pending requests.
func (s *Service) Run(ctx context.Context) {
	if s.NodeID == "" || s.Boot == "" || s.Target == nil {
		return
	}
	wake := make(chan struct{}, 1)
	if s.Bus != nil {
		defer s.Bus.Subscribe(Topic, func([]byte) {
			select {
			case wake <- struct{}{}:
			default:
			}
		})()
		defer s.Bus.Subscribe(clusterbus.TopicResync, func([]byte) {
			select {
			case wake <- struct{}{}:
			default:
			}
		})()
	}
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		}
	}
}
func (s *Service) reconcile(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	obs, caps := s.Target.Observe()
	initial := document{Control: Control{NodeID: s.NodeID, Boot: s.Boot, State: "idle", UpdatedAt: time.Now().UTC(), Observed: obs, Capabilities: caps}}
	raw, _ := json.Marshal(initial)
	if s.Store.Initialize(ctx, s.NodeID, s.Boot, raw) != nil {
		return
	}
	d, v, err := s.load(ctx, s.NodeID, s.Boot)
	if err != nil {
		return
	}
	if d.State == "pending" {
		code := s.Target.Apply(ctx, d.Patch)
		d.ErrorCode = code
		if code != "" {
			d.State = "failed"
		} else {
			d.State = "applied"
			d.AppliedVersion = d.RequestedVersion
		}
	}
	d.Observed, d.Capabilities = s.Target.Observe()
	d.UpdatedAt = time.Now().UTC()
	raw, _ = json.Marshal(d)
	_ = s.Store.CompareAndSwap(ctx, s.NodeID, s.Boot, v, raw)
}

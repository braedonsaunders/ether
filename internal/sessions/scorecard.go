//go:build scorecard && !plexparity

package sessions

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// ScorecardSession exposes only fixture correlation and observed delivery state.
type ScorecardSession struct {
	ID                 string                `json:"id"`
	PlaybackID         string                `json:"playback_id"`
	ClientID           string                `json:"client_id"`
	PlanDecision       core.Decision         `json:"plan_decision"`
	Format             frontend.StreamFormat `json:"format"`
	TranscoderActive   bool                  `json:"transcoder_active"`
	TranscoderDecision *core.Decision        `json:"transcoder_decision,omitempty"`
}

func (b *Broker) ScorecardSessions(ctx context.Context) []ScorecardSession {
	b.mu.Lock()
	rows := make([]ScorecardSession, 0, len(b.sessions))
	for id, s := range b.sessions {
		rows = append(rows, ScorecardSession{ID: id, PlaybackID: s.playID, ClientID: s.clientID, PlanDecision: s.decision, Format: s.format})
	}
	b.mu.Unlock()
	for n := range rows {
		hs, ok := b.TranscodeFor(ctx, rows[n].PlaybackID)
		if !ok {
			continue
		}
		rows[n].TranscoderActive = true
		if d, ok := hs.(interface{ Decision() core.Decision }); ok {
			decision := d.Decision()
			rows[n].TranscoderDecision = &decision
		}
	}
	return rows
}

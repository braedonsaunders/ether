package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"time"
)

func init() {
	store.Register("plex_decision_arms", 1, `CREATE TABLE plex_decision_arms(user_id BIGINT NOT NULL,client_id TEXT NOT NULL,session_id TEXT NOT NULL,title_key BIGINT NOT NULL,expires BIGINT NOT NULL,PRIMARY KEY(user_id,client_id,session_id,title_key)); CREATE INDEX plex_decision_arms_expiry ON plex_decision_arms(expires);`)
	store.Register("plex_decision_arms", 2, `ALTER TABLE plex_decision_arms ADD COLUMN node_id TEXT NOT NULL DEFAULT ''; CREATE INDEX plex_decision_arms_node ON plex_decision_arms(node_id);`)
	store.Register("plex_decision_arms", 3, `ALTER TABLE plex_decision_arms ADD COLUMN active INTEGER NOT NULL DEFAULT 1;`)
	store.RegisterViewerTable("plex_decision_arms", "plex_decision_arms", "user_id", store.ViewerMergeDrop)
}

type decisionArm struct {
	user            int64
	client, session string
	items           map[core.RatingKey]bool
	at              time.Time
}

// The public player session owns its decision state; the encoder's session=
// value is an independent delivery handle and cannot authorize another player.
func (s *Server) armDecision(pr *Request, key core.RatingKey) {
	if pr.User == nil || key == 0 {
		return
	}
	s.termMu.Lock()
	if s.decisionArms == nil {
		s.decisionArms = make(map[string]decisionArm)
	}
	id := trackID(pr.User.ID, pr.ClientID, pr.SessionID)
	a := s.decisionArms[id]
	if a.items == nil {
		a.items = make(map[core.RatingKey]bool)
	}
	a.user, a.client, a.session = pr.User.ID, pr.ClientID, pr.SessionID
	a.items[key] = true
	a.at = s.now()
	s.decisionArms[id] = a
	s.termMu.Unlock()
	s.persistDecisionArm(pr, key)
}
func (s *Server) decisionArmed(pr *Request, key core.RatingKey) bool {
	if pr.User == nil {
		return false
	}
	id := trackID(pr.User.ID, pr.ClientID, pr.SessionID)
	s.termMu.Lock()
	a, ok := s.decisionArms[id]
	if ok && s.now().Sub(a.at) <= 3*time.Minute && (key == 0 || a.items[key]) {
		a.at = s.now()
		s.decisionArms[id] = a
		s.termMu.Unlock()
		if s.d.DB == nil {
			return true
		}
	} else {
		s.termMu.Unlock()
	}
	if s.d.DB == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(s.ctx, dbTimeout)
	defer cancel()
	query := `SELECT 1 FROM plex_decision_arms WHERE user_id=? AND client_id=? AND session_id=? AND expires>=?`
	args := []any{pr.User.ID, pr.ClientID, pr.SessionID, s.now().UnixMilli()}
	if key != 0 {
		query += ` AND title_key=? AND active=1`
		args = append(args, key)
	}
	query += ` LIMIT 1`
	var found int
	return s.d.DB.QueryRowContext(ctx, query, args...).Scan(&found) == nil
}

// Stopping one item clears its start/bandwidth authorization while retaining
// the session's history of having made a decision.
func (s *Server) disarmDecision(pr *Request, key core.RatingKey) {
	if pr.User == nil {
		return
	}
	id := trackID(pr.User.ID, pr.ClientID, pr.SessionID)
	s.termMu.Lock()
	if a, ok := s.decisionArms[id]; ok {
		if _, seen := a.items[key]; seen {
			a.items[key] = false
			s.decisionArms[id] = a
		}
	}
	s.termMu.Unlock()
	s.persistDecisionChange(pr, key, true)
}

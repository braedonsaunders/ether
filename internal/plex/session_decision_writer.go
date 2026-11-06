package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

type decisionArmWrite struct {
	disarm          bool
	user            int64
	client, session string
	key             core.RatingKey
	expires         int64
	done            chan struct{}
}

// Decisions acknowledge a background batch before returning, so an immediate
// start on another node observes the arm. Rendering and starts only read it.
func (s *Server) persistDecisionArm(pr *Request, key core.RatingKey) {
	s.persistDecisionChange(pr, key, false)
}
func (s *Server) persistDecisionChange(pr *Request, key core.RatingKey, disarm bool) {
	if s.d.DB == nil {
		return
	}
	s.decisionWriterOnce.Do(func() {
		s.decisionWriter = make(chan decisionArmWrite, 64)
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.writeDecisionArms() }()
	})
	cmd := decisionArmWrite{disarm: disarm, user: pr.User.ID, client: pr.ClientID, session: pr.SessionID, key: key, expires: s.now().Add(3 * time.Minute).UnixMilli(), done: make(chan struct{})}
	select {
	case s.decisionWriter <- cmd:
	case <-s.ctx.Done():
		return
	}
	select {
	case <-cmd.done:
	case <-s.ctx.Done():
	}
}

func (s *Server) writeDecisionArms() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case first := <-s.decisionWriter:
			batch := []decisionArmWrite{first}
		drain:
			for len(batch) < 64 {
				select {
				case cmd := <-s.decisionWriter:
					batch = append(batch, cmd)
				default:
					break drain
				}
			}
			ctx, cancel := context.WithTimeout(s.ctx, dbTimeout)
			tx, err := s.d.DB.BeginTx(ctx, nil)
			if err == nil {
				for _, cmd := range batch {
					if cmd.disarm {
						_, err = tx.ExecContext(ctx, `UPDATE plex_decision_arms SET active=0 WHERE user_id=? AND client_id=? AND session_id=? AND title_key=?`, cmd.user, cmd.client, cmd.session, cmd.key)
						if err != nil {
							break
						}
						continue
					}
					_, err = tx.ExecContext(ctx, `INSERT INTO plex_decision_arms(user_id,client_id,session_id,title_key,expires,node_id,active) VALUES(?,?,?,?,?,?,1) ON CONFLICT(user_id,client_id,session_id,title_key) DO UPDATE SET active=1,expires=CASE WHEN plex_decision_arms.expires>excluded.expires THEN plex_decision_arms.expires ELSE excluded.expires END,node_id=CASE WHEN plex_decision_arms.expires>excluded.expires THEN plex_decision_arms.node_id ELSE excluded.node_id END`, cmd.user, cmd.client, cmd.session, cmd.key, cmd.expires, s.decisionNode())
					if err != nil {
						break
					}
				}
				if err == nil {
					_, err = tx.ExecContext(ctx, `DELETE FROM plex_decision_arms WHERE expires<?`, s.now().UnixMilli())
				}
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			cancel()
			if err != nil {
				s.log.Warn("persist playback decision arms", "err", err)
			}
			for _, cmd := range batch {
				close(cmd.done)
			}
		}
	}
}

// The session janitor refreshes live arms and removes expired durable entries.
// Timeline and read handlers update only memory; SQLite writes stay here.
func (s *Server) refreshDecisionArms(now time.Time) {
	if s.d.DB == nil {
		return
	}
	s.termMu.Lock()
	rows := make([]decisionArmWrite, 0, len(s.decisionArms))
	for _, arm := range s.decisionArms {
		if now.Sub(arm.at) > 3*time.Minute {
			continue
		}
		for key, active := range arm.items {
			rows = append(rows, decisionArmWrite{disarm: !active, user: arm.user, client: arm.client, session: arm.session, key: key, expires: arm.at.Add(3 * time.Minute).UnixMilli()})
		}
	}
	s.termMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, dbTimeout)
	defer cancel()
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	for _, row := range rows {
		active := 1
		if row.disarm {
			active = 0
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO plex_decision_arms(user_id,client_id,session_id,title_key,expires,node_id,active) VALUES(?,?,?,?,?,?,?) ON CONFLICT(user_id,client_id,session_id,title_key) DO UPDATE SET expires=CASE WHEN plex_decision_arms.expires>excluded.expires THEN plex_decision_arms.expires ELSE excluded.expires END,node_id=CASE WHEN plex_decision_arms.expires>excluded.expires THEN plex_decision_arms.node_id ELSE excluded.node_id END`, row.user, row.client, row.session, row.key, row.expires, s.decisionNode(), active); err != nil {
			return
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM plex_decision_arms WHERE expires<?`, now.UnixMilli()); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		s.log.Warn("refresh playback decision arms", "err", err)
	}
}

func (s *Server) decisionNode() string {
	if s.cl != nil {
		return s.cl.self
	}
	return "standalone"
}

func (s *Server) clearOwnDecisionArms() {
	if s.d.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if _, err := s.d.DB.ExecContext(ctx, `DELETE FROM plex_decision_arms WHERE node_id=?`, s.decisionNode()); err != nil {
		s.log.Warn("clear playback decision arms", "err", err)
	}
}

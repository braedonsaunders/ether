package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Channel access has one source: each viewer's Policy. The owner edits it on
// the viewer's page (their allowed channels) and on each channel (which
// viewers may watch it); both write the same policies.

// Permits reports whether the policy lets its viewer watch channel id, which
// is private when only granted viewers may watch it. Rating and library
// limits are checked separately.
func (p Policy) Permits(id string, private bool) bool {
	if p.Disabled {
		return false
	}
	if !p.Restricted {
		return !private || slices.Contains(p.Granted, id)
	}
	if p.channelSet != nil {
		_, ok := p.channelSet[id]
		return ok
	}
	return slices.Contains(p.Channels, id)
}

// normalize drops duplicates, the channel list of an unrestricted viewer (who
// sees every public channel) and the grants of a restricted one (whose list
// already names every channel they see).
func (p *Policy) normalize() {
	if p.Restricted {
		p.Channels = uniqueIDs(p.Channels)
		p.Granted = nil
		return
	}
	p.Channels = nil
	p.Granted = uniqueIDs(p.Granted)
}

func uniqueIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if _, dup := seen[id]; id == "" || dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// indexChannels makes Permits constant time for long allow-lists: a guide
// checks every channel against the same policy.
func (p *Policy) indexChannels() {
	if len(p.Channels) < 16 {
		return
	}
	p.channelSet = make(map[string]struct{}, len(p.Channels))
	for _, id := range p.Channels {
		p.channelSet[id] = struct{}{}
	}
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func savePolicy(ctx context.Context, db execer, p Policy) error {
	p.normalize()
	p.channelSet = nil
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO livetv_policies(user_id,data) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET data=excluded.data`, p.UserID, b)
	return err
}

func readPolicy(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID int64) (Policy, error) {
	p := Policy{UserID: userID}
	var b []byte
	err := db.QueryRowContext(ctx, `SELECT data FROM livetv_policies WHERE user_id=?`, userID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &p)
		p.UserID = userID
	}
	return p, err
}

// Policies lists every stored viewer policy. Viewers without one see every
// channel.
func (s *Service) Policies(ctx context.Context) ([]Policy, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT user_id, data FROM livetv_policies ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Policy{}
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		var p Policy
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		p.UserID = id
		out = append(out, p)
	}
	return out, rows.Err()
}

// ChannelViewers changes which viewers may watch one channel by editing their
// policies. Allowing a restricted viewer adds the channel to their list;
// allowing an unrestricted one grants a private channel (a public one they
// already see). Denying a private channel removes it from the list or the
// grants. Denying a public channel to an unrestricted viewer restricts them to
// every other channel they can see now, so they stop receiving new channels
// automatically. A viewer with Live TV disabled keeps it disabled.
type ChannelViewers struct {
	Allow []int64 `json:"allow,omitempty"`
	Deny  []int64 `json:"deny,omitempty"`
}

func (s *Service) SetChannelViewers(ctx context.Context, channelID string, change ChannelViewers) error {
	for _, id := range append(slices.Clone(change.Allow), change.Deny...) {
		if id <= 0 {
			return fmt.Errorf("viewer ids must be positive")
		}
	}
	if slices.ContainsFunc(change.Allow, func(id int64) bool { return slices.Contains(change.Deny, id) }) {
		return fmt.Errorf("a viewer cannot be both allowed and denied")
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.LockKey(ctx, store.DialectOf(s.o.DB), tx, "livetv_policies"); err != nil {
		return err
	}
	channels, err := channelAccess(ctx, tx)
	if err != nil {
		return err
	}
	private, found := channels[channelID]
	if !found {
		return core.ErrNotFound
	}
	for _, id := range change.Allow {
		p, err := readPolicy(ctx, tx, id)
		if err != nil {
			return err
		}
		// Live TV switched off stays off; the grant still applies when it is back.
		enabled := p
		enabled.Disabled = false
		switch {
		case enabled.Permits(channelID, private):
			continue
		case p.Restricted:
			p.Channels = append(p.Channels, channelID)
		default:
			p.Granted = append(p.Granted, channelID)
		}
		if err := savePolicy(ctx, tx, p); err != nil {
			return err
		}
	}
	for _, id := range change.Deny {
		p, err := readPolicy(ctx, tx, id)
		if err != nil {
			return err
		}
		drop := func(c string) bool { return c == channelID }
		switch {
		case p.Restricted:
			if !slices.Contains(p.Channels, channelID) {
				continue
			}
			p.Channels = slices.DeleteFunc(p.Channels, drop)
		case private:
			if !slices.Contains(p.Granted, channelID) {
				continue
			}
			p.Granted = slices.DeleteFunc(p.Granted, drop)
		default:
			// Everything they see today except this channel.
			seen := []string{}
			enabled := p
			enabled.Disabled = false
			for _, c := range sortedKeys(channels) {
				if c != channelID && enabled.Permits(c, channels[c]) {
					seen = append(seen, c)
				}
			}
			p.Restricted = true
			p.Channels = seen
		}
		if err := savePolicy(ctx, tx, p); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.refreshVirtualGuide(ctx); err != nil {
		return err
	}
	s.bumpLineup()
	return nil
}

// channelAccess maps every channel id to whether it is private.
func channelAccess(ctx context.Context, db queryer) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, data FROM livetv_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		var c struct {
			Private bool `json:"private"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("live TV channel %s: %w", id, err)
		}
		out[id] = c.Private
	}
	return out, rows.Err()
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// VisibleTo reports whether Live TV should appear in u's apps at all: the
// server has at least one channel and the owner has not turned Live TV off
// for this viewer. The owner's own switch applies to the owner's apps too.
// It is two indexed lookups, cheap enough for the provider list apps fetch
// on every launch.
func (s *Service) VisibleTo(ctx context.Context, u *core.User) bool {
	if u == nil {
		return false
	}
	var one int
	if err := s.o.DB.QueryRowContext(ctx, `SELECT 1 FROM livetv_channels LIMIT 1`).Scan(&one); err != nil {
		return false
	}
	_, pol, err := s.withPolicy(ctx, u)
	return err == nil && !pol.Disabled
}

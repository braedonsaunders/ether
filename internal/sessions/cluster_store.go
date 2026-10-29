package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// clusterStore uses historical table names with protocol-neutral content.
// The data column retains Plex's legacy shared-entry JSON shape for old nodes.
type clusterStore struct {
	db   *sql.DB
	node string
}

type legacyClusterEntry struct {
	ID         string         `json:"id"`
	SessionKey int64          `json:"sessionKey"`
	User       core.User      `json:"user"`
	Client     legacyClient   `json:"client"`
	Key        core.RatingKey `json:"key"`
	State      string         `json:"state"`
	StartedAt  time.Time      `json:"startedAt"`
	UpdatedAt  time.Time      `json:"updatedAt"`
	SourceID   string         `json:"sourceID,omitempty"`
	Channel    string         `json:"channel,omitempty"`
}
type legacyClient struct {
	ClientID   string `json:"clientID"`
	Product    string `json:"product,omitempty"`
	Version    string `json:"version,omitempty"`
	Platform   string `json:"platform,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`
}

func (c clusterStore) put(ctx context.Context, s frontend.Session, sourceID string, viewer core.Viewer, now time.Time) error {
	var key int64
	if err := c.db.QueryRowContext(ctx, `UPDATE plex_counters SET value=value+1 WHERE name='session_key' RETURNING value`).Scan(&key); err != nil {
		return err
	}
	// Never persist a media grant or credential. Only identity needed by old
	// status readers is written; policy and credentials remain authoritative.
	v := core.User{ID: viewer.ID, Username: viewer.Username, Title: viewer.Title, IsOwner: viewer.IsOwner}
	d := s.Device
	d.Client = ""
	data, err := json.Marshal(legacyClusterEntry{ID: s.ID, SessionKey: key, User: v, Client: legacyClient{ClientID: d.ID, Product: d.Product, Version: d.Version, Platform: d.Platform, DeviceName: d.DeviceName}, Key: s.Item, State: "playing", StartedAt: s.StartedAt, UpdatedAt: now, SourceID: sourceID, Channel: s.Channel})
	if err != nil {
		return err
	}
	device, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO plex_sessions(id,node_id,user_id,session_key,data,updated_at,protocol,surface,device_id,device_data) VALUES(?,?,?,?,?,?,?,?,?,?)`, s.ID, c.node, s.ViewerID, key, string(data), now.UnixMilli(), s.Protocol, s.Protocol, d.ID, string(device))
	return err
}

// cleanup never removes Plex rows, including legacy rows with NULL protocol.
func (c clusterStore) cleanup(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM plex_sessions WHERE node_id=? AND COALESCE(protocol,'plex')<>'plex'`, c.node)
	return err
}

func (c clusterStore) expire(ctx context.Context, cutoff time.Time) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM plex_sessions WHERE COALESCE(protocol,'plex')<>'plex' AND updated_at<?`, cutoff.UnixMilli())
	return err
}

func (c clusterStore) remove(ctx context.Context, id string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM plex_sessions WHERE id=? AND node_id=? AND COALESCE(protocol,'plex')<>'plex'`, id, c.node)
	return err
}

func (c clusterStore) touch(ctx context.Context, id string, now time.Time) error {
	_, err := c.db.ExecContext(ctx, `UPDATE plex_sessions SET updated_at=? WHERE id=? AND node_id=? AND COALESCE(protocol,'plex')<>'plex'`, now.UnixMilli(), id, c.node)
	return err
}

type remoteSession struct {
	session  frontend.Session
	node     string
	sourceID string
}

func (c clusterStore) list(ctx context.Context, cutoff time.Time, live map[string]bool) ([]remoteSession, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT node_id,user_id,data,protocol,device_data FROM plex_sessions WHERE node_id<>? AND COALESCE(protocol,'plex')<>'plex' AND updated_at>=?`, c.node, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []remoteSession
	for rows.Next() {
		var node, data, protocol string
		var device sql.NullString
		var viewer int64
		if err := rows.Scan(&node, &viewer, &data, &protocol, &device); err != nil {
			return nil, err
		}
		if live != nil && !live[node] {
			continue
		}
		var e legacyClusterEntry
		var d core.Device
		if json.Unmarshal([]byte(data), &e) != nil || !device.Valid || json.Unmarshal([]byte(device.String), &d) != nil || e.ID == "" || e.User.ID != viewer {
			continue
		}
		d.Client = ""
		out = append(out, remoteSession{node: node, sourceID: e.SourceID, session: frontend.Session{ID: e.ID, ViewerID: viewer, Protocol: frontend.Protocol(protocol), Item: e.Key, Channel: e.Channel, Device: d, StartedAt: e.StartedAt}})
	}
	return out, rows.Err()
}

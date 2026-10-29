package notify

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("viewer_push", 1, `CREATE TABLE viewer_push_subscriptions (endpoint_hash TEXT PRIMARY KEY, viewer_id INTEGER NOT NULL, subscription TEXT NOT NULL, created_at INTEGER NOT NULL, grant_hash TEXT NOT NULL);
CREATE INDEX viewer_push_subscriptions_viewer ON viewer_push_subscriptions(viewer_id);
CREATE TABLE viewer_push_claims (viewer_id INTEGER NOT NULL, episode_key TEXT NOT NULL, endpoint_hash TEXT NOT NULL, claimed_at INTEGER NOT NULL, PRIMARY KEY(viewer_id,episode_key,endpoint_hash));`)
}

// ViewerPush uses viewer-owned destinations, separate from owner channels.
// Allowed must recheck the favorite show and current episode/show policy.
type ViewerPush struct {
	Notifications *Service
	Preferences   *ViewerAlerts
	Allowed       func(context.Context, *core.User, core.RatingKey) (bool, error)
	Now           func() time.Time
	// Send is optional and permits offline tests; nil uses the web-push transport.
	Send func(context.Context, PushSubscription, Event) error
}

func (p *ViewerPush) ready() bool {
	return p != nil && p.Notifications != nil && p.Preferences != nil && p.Preferences.DB != nil && p.Preferences.DB == p.Notifications.db && p.Preferences.Active != nil
}
func (p *ViewerPush) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
func pushHash(endpoint string) string {
	h := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(h[:])
}
func viewerPushContext(id int64, hash string) []string {
	return []string{"viewer_push_subscriptions", strconv.FormatInt(id, 10), hash}
}

func (p *ViewerPush) PublicKey(ctx context.Context, viewer *core.User) (string, error) {
	if !p.ready() {
		return "", core.ErrUnauthorized
	}
	if err := p.Preferences.authorized(ctx, viewer); err != nil {
		return "", err
	}
	_, public, err := p.Notifications.pushKeys(ctx)
	return public, err
}

// Register associates a browser endpoint with its current viewer only.
func (p *ViewerPush) Register(ctx context.Context, viewer *core.User, subscription PushSubscription, grantHash string) error {
	if !p.ready() {
		return core.ErrUnauthorized
	}
	if err := p.Preferences.authorized(ctx, viewer); err != nil {
		return err
	}
	if err := validatePush(&subscription); err != nil {
		return err
	}
	if len(grantHash) != 64 {
		return core.ErrUnauthorized
	}
	if len(subscription.Endpoint) > 4096 {
		return errors.New("push endpoint too long")
	}
	hash := pushHash(subscription.Endpoint)
	data, err := json.Marshal(subscription)
	if err != nil {
		return err
	}
	sealed, err := config.SealWith(p.Notifications.opts.Sealer, string(data), viewerPushContext(viewer.ID, hash)...)
	if err != nil {
		return err
	}
	res, err := p.Preferences.DB.ExecContext(ctx, `INSERT INTO viewer_push_subscriptions(endpoint_hash,viewer_id,subscription,created_at,grant_hash) SELECT ?,?,?,?,? WHERE EXISTS (SELECT 1 FROM identity_grants WHERE hash=? AND viewer_id=? AND surface='viewer-portal' AND revoked=0 AND expires>?) ON CONFLICT(endpoint_hash) DO UPDATE SET subscription=excluded.subscription, grant_hash=excluded.grant_hash, created_at=excluded.created_at WHERE viewer_push_subscriptions.viewer_id=excluded.viewer_id`, hash, viewer.ID, sealed, p.now().Unix(), grantHash, grantHash, viewer.ID, p.now().UnixMilli())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return core.ErrUnauthorized
	}
	return nil
}

func (p *ViewerPush) Unregister(ctx context.Context, viewer *core.User, endpoint string) error {
	if !p.ready() {
		return core.ErrUnauthorized
	}
	if err := p.Preferences.authorized(ctx, viewer); err != nil {
		return err
	}
	_, err := p.Preferences.DB.ExecContext(ctx, `DELETE FROM viewer_push_subscriptions WHERE viewer_id=? AND endpoint_hash=?`, viewer.ID, pushHash(endpoint))
	return err
}

// Registered reports whether this exact browser endpoint belongs to the viewer.
func (p *ViewerPush) Registered(ctx context.Context, viewer *core.User, endpoint string) (bool, error) {
	if !p.ready() {
		return false, core.ErrUnauthorized
	}
	if err := p.Preferences.authorized(ctx, viewer); err != nil {
		return false, err
	}
	var found int
	err := p.Preferences.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM viewer_push_subscriptions WHERE viewer_id=? AND endpoint_hash=?)`, viewer.ID, pushHash(endpoint)).Scan(&found)
	return found != 0, err
}

// PurgeViewer removes all destinations after membership deactivation or revocation.
func (p *ViewerPush) PurgeViewer(ctx context.Context, viewerID int64) error {
	if !p.ready() || viewerID <= 0 {
		return core.ErrUnauthorized
	}
	_, err := p.Preferences.DB.ExecContext(ctx, `DELETE FROM viewer_push_subscriptions WHERE viewer_id=?`, viewerID)
	return err
}

// PurgeViewerTx and PurgeGrantTx participate in the same transaction that
// deactivates membership or revokes a device grant.
func PurgeViewerTx(ctx context.Context, tx *sql.Tx, viewerID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM viewer_push_subscriptions WHERE viewer_id=?`, viewerID)
	return err
}
func PurgeGrantTx(ctx context.Context, tx *sql.Tx, viewerID int64, grantHash string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM viewer_push_subscriptions WHERE viewer_id=? AND grant_hash=?`, viewerID, grantHash)
	return err
}

// Drain claims recent alerts once per destination across nodes. Claims are
// intentionally at-most-once: a crash or send failure does not replay a push.
// Old alerts and alerts preceding subscription or opt-in are never sent.
func (p *ViewerPush) Drain(ctx context.Context) error {
	if !p.ready() || p.Allowed == nil {
		return core.ErrUnauthorized
	}
	now := p.now()
	viewers, err := p.Preferences.DB.QueryContext(ctx, `SELECT DISTINCT viewer_id FROM viewer_push_subscriptions`)
	if err != nil {
		return err
	}
	var ids []int64
	for viewers.Next() {
		var id int64
		if err = viewers.Scan(&id); err != nil {
			viewers.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = viewers.Err()
	viewers.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		u, err := p.Preferences.Active(ctx, id)
		if err != nil && !errors.Is(err, core.ErrUnauthorized) && !errors.Is(err, core.ErrNotFound) {
			return err
		}
		if err != nil || u == nil || u.ID != id {
			if e := p.PurgeViewer(ctx, id); e != nil {
				return e
			}
		}
	}
	rows, err := p.Preferences.DB.QueryContext(ctx, `SELECT a.viewer_id,a.episode_key,a.show_title,a.episode_title,s.endpoint_hash,s.subscription FROM viewer_episode_alerts a JOIN viewer_push_subscriptions s ON s.viewer_id=a.viewer_id JOIN viewer_alert_preferences p ON p.viewer_id=a.viewer_id JOIN identity_grants g ON g.hash=s.grant_hash AND g.viewer_id=s.viewer_id AND g.surface='viewer-portal' AND g.revoked=0 AND g.expires>? LEFT JOIN viewer_push_claims c ON c.viewer_id=a.viewer_id AND c.episode_key=a.episode_key AND c.endpoint_hash=s.endpoint_hash WHERE c.viewer_id IS NULL AND p.enabled=1 AND a.detected_at>? AND a.detected_at<=? AND a.detected_at>s.created_at AND a.preference_generation=p.enabled_at ORDER BY a.detected_at LIMIT 1000`, now.UnixMilli(), now.Add(-time.Hour).Unix(), now.Unix())
	if err != nil {
		return err
	}
	type pending struct {
		id                               int64
		key, show, episode, hash, sealed string
	}
	var pendingRows []pending
	for rows.Next() {
		var v pending
		if err = rows.Scan(&v.id, &v.key, &v.show, &v.episode, &v.hash, &v.sealed); err != nil {
			rows.Close()
			return err
		}
		pendingRows = append(pendingRows, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range pendingRows {
		viewer, err := p.Preferences.Active(ctx, item.id)
		if err != nil && !errors.Is(err, core.ErrUnauthorized) && !errors.Is(err, core.ErrNotFound) {
			return err
		}
		if err != nil || viewer == nil || viewer.ID != item.id {
			if e := p.PurgeViewer(ctx, item.id); e != nil {
				return e
			}
			continue
		}
		enabled, err := p.Preferences.Enabled(ctx, viewer)
		if err != nil {
			return err
		}
		if !enabled {
			continue
		}
		key, err := core.ParseRatingKey(item.key)
		if err != nil || key.Kind() != core.KindEpisode {
			continue
		}
		allowed, err := p.Allowed(ctx, viewer, key)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		plain, _, err := config.OpenWith(p.Notifications.opts.Sealer, item.sealed, viewerPushContext(item.id, item.hash)...)
		if err != nil {
			return err
		}
		var subscription PushSubscription
		if json.Unmarshal([]byte(plain), &subscription) != nil || validatePush(&subscription) != nil || pushHash(subscription.Endpoint) != item.hash {
			continue
		}
		res, err := p.Preferences.DB.ExecContext(ctx, `INSERT INTO viewer_push_claims(viewer_id,episode_key,endpoint_hash,claimed_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, item.id, item.key, item.hash, now.Unix())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		// Revalidate after claiming, immediately before delivery.
		viewer, err = p.Preferences.Active(ctx, item.id)
		if err != nil && !errors.Is(err, core.ErrUnauthorized) && !errors.Is(err, core.ErrNotFound) {
			return err
		}
		if err != nil || viewer == nil || viewer.ID != item.id {
			if e := p.PurgeViewer(ctx, item.id); e != nil {
				return e
			}
			continue
		}
		enabled, err = p.Preferences.Enabled(ctx, viewer)
		if err != nil {
			return err
		}
		if !enabled {
			continue
		}
		allowed, err = p.Allowed(ctx, viewer, key)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		var current int
		err = p.Preferences.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM viewer_push_subscriptions s JOIN viewer_alert_preferences p ON p.viewer_id=s.viewer_id JOIN identity_grants g ON g.hash=s.grant_hash AND g.viewer_id=s.viewer_id AND g.surface='viewer-portal' AND g.revoked=0 AND g.expires>? JOIN viewer_episode_alerts a ON a.viewer_id=s.viewer_id AND a.episode_key=? WHERE s.viewer_id=? AND s.endpoint_hash=? AND s.subscription=? AND p.enabled=1 AND p.enabled_at=a.preference_generation AND s.created_at<a.detected_at)`, now.UnixMilli(), item.key, item.id, item.hash, item.sealed).Scan(&current)
		if err != nil {
			return err
		}
		if current == 0 {
			continue
		}
		event := Event{Title: item.show, Message: item.episode, Kind: "viewer.episode", At: now}
		if p.Send != nil {
			_ = p.Send(ctx, subscription, event)
		} else {
			_ = p.send(ctx, subscription, event)
		}
	}
	_, err = p.Preferences.DB.ExecContext(ctx, `DELETE FROM viewer_push_claims WHERE claimed_at<?`, now.Add(-48*time.Hour).Unix())
	return err
}

func (p *ViewerPush) send(ctx context.Context, subscription PushSubscription, event Event) error {
	private, public, err := p.Notifications.pushKeys(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"title": clip(event.Title, 150), "message": clip(event.Message, 450), "url": "/ether/me"})
	if err != nil {
		return err
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{Endpoint: subscription.Endpoint, Keys: webpush.Keys{Auth: subscription.Keys.Auth, P256dh: subscription.Keys.P256dh}}, &webpush.Options{HTTPClient: p.Notifications.opts.Client, Subscriber: "https://ether.local", VAPIDPrivateKey: private, VAPIDPublicKey: public, TTL: 3600, Urgency: webpush.UrgencyHigh})
	if err != nil {
		return errors.New("browser push transport failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("browser push service rejected notification")
	}
	return nil
}

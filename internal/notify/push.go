package notify

import (
	"context"
	"crypto/ecdh"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

type PushKeys struct {
	Auth   string `json:"auth"`
	P256dh string `json:"p256dh"`
}
type PushSubscription struct {
	Endpoint string   `json:"endpoint"`
	Keys     PushKeys `json:"keys"`
}

func validatePush(p *PushSubscription) error {
	if p == nil {
		return errors.New("browser subscription required; connect this device from Notifications")
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("HTTPS push endpoint required")
	}
	auth, err := base64.RawURLEncoding.DecodeString(p.Keys.Auth)
	if err != nil || len(auth) != 16 {
		return errors.New("invalid browser authentication key")
	}
	pub, err := base64.RawURLEncoding.DecodeString(p.Keys.P256dh)
	if err != nil {
		return errors.New("invalid browser encryption key")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("invalid browser encryption key")
	}
	return nil
}

// validateViewerPush extends validatePush for non-admin viewer subscriptions,
// rejecting loopback, local-domain and non-public IP literal endpoints.
func validateViewerPush(p *PushSubscription) error {
	if err := validatePush(p); err != nil {
		return err
	}
	u, _ := url.Parse(p.Endpoint)
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errors.New("public HTTPS push endpoint required")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !safefetch.IsPublicIP(ip) {
		return errors.New("public HTTPS push endpoint required")
	}
	return nil
}

// InitializePush provisions the durable identity before serving public-key reads.
func (s *Service) InitializePush(ctx context.Context) error {
	_, _, err := s.pushKeys(ctx)
	return err
}

func (s *Service) readPushKeys(ctx context.Context) (private, public string, err error) {
	if s.pushPrivate != "" {
		return s.pushPrivate, s.pushPublic, nil
	}
	if err = s.db.QueryRowContext(ctx, `SELECT private_key,public_key FROM notify_push_keys WHERE id='vapid'`).Scan(&private, &public); err != nil {
		return "", "", err
	}
	if private, _, err = config.OpenWith(s.opts.Sealer, private, pushKeyContext(public)...); err != nil {
		return "", "", fmt.Errorf("notify: push identity: %w", err)
	}
	return private, public, nil
}

// One durable VAPID identity is shared across nodes and survives restarts.
func (s *Service) pushKeys(ctx context.Context) (private, public string, err error) {
	private, public, err = s.readPushKeys(ctx)
	if !errors.Is(err, sql.ErrNoRows) {
		return private, public, err
	}
	private, public, err = webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", err
	}
	sealed, err := config.SealWith(s.opts.Sealer, private, pushKeyContext(public)...)
	if err != nil {
		return "", "", err
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO notify_push_keys(id,private_key,public_key) VALUES('vapid',?,?) ON CONFLICT(id) DO NOTHING`, sealed, public); err != nil {
		return "", "", err
	}
	return s.readPushKeys(ctx)
}
func (s *Service) sendPush(ctx context.Context, c Channel, e Event) error {
	sub, err := c.Subscription()
	if err != nil {
		return err
	}
	if err := validatePush(sub); err != nil {
		return err
	}
	private, public, err := s.pushKeys(ctx)
	if err != nil {
		return errors.New("browser push identity unavailable")
	}
	payload, _ := json.Marshal(map[string]string{"title": clip(e.Title, 150), "message": clip(e.Message, 450), "url": "/ether/notifications/deliveries"})
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{Auth: sub.Keys.Auth, P256dh: sub.Keys.P256dh}}, &webpush.Options{HTTPClient: s.opts.Client, Subscriber: "https://ether.local", VAPIDPrivateKey: private, VAPIDPublicKey: public, TTL: 3600, Urgency: webpush.UrgencyHigh})
	if err != nil {
		return errors.New("browser push transport failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	if resp.StatusCode == 404 || resp.StatusCode == 410 {
		return errors.New("browser subscription expired; reconnect this device")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("browser push service rejected the notification")
	}
	return nil
}

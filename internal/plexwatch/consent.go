package plexwatch

import (
	"context"
	"time"
)

// The account's "Sync My Watch State and Ratings" setting. Plex does not
// enforce it on the Discover routes, so Ether does: while it is off nothing
// is read from or sent to that account. It is read once per viewer per
// pass (a scheduled pass, or a delivery pass before its first send for that
// viewer), never per title, so turning it on in Plex starts sync without
// reconnecting.
const (
	// consentParkReason marks journal rows held while the setting is off.
	consentParkReason = "consent_off"
	// consentPark is how long held rows wait before they are looked at
	// again. Rows are woken as soon as any pass reads the setting on, so
	// this only bounds a missed wake.
	consentPark = 24 * time.Hour
	// consentOffReuse lets a delivery pass trust an "off" read this recent
	// instead of asking Plex again for every newly queued change.
	consentOffReuse = time.Minute
)

// sendPass is one delivery pass: the consent read per viewer is shared by
// every send in the pass.
type sendPass struct {
	consent map[int64]bool
}

func newSendPass() *sendPass { return &sendPass{consent: map[int64]bool{}} }

// readConsent asks Plex for the setting and records the answer for the
// account generation that asked. A setting found on wakes the viewer's
// held changes. Errors are Plex's: ErrRevoked for 401/403, StatusError or
// transport errors otherwise; the caller classifies them.
func (s *Service) readConsent(ctx context.Context, acct accountRow) (bool, error) {
	on, err := s.client.consent(ctx, acct.token)
	if err != nil {
		return false, err
	}
	v := 0
	if on {
		v = 1
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE plexwatch_account SET consent=?, consent_checked=? WHERE user_id=? AND token_generation=?`,
		v, s.now().UnixNano(), acct.userID, acct.tokenGeneration); err != nil {
		return false, err
	}
	if on {
		if err := s.state.WakeSync(ctx, acct.userID, consentParkReason); err != nil {
			return false, err
		}
	} else if acct.consent != 0 {
		s.log.Info("plex account has watch-state sync turned off", "user", acct.userID)
	}
	return on, nil
}

// passConsent is readConsent at most once per viewer per delivery pass. A
// recent "off" is reused without asking Plex.
func (s *Service) passConsent(ctx context.Context, pass *sendPass, acct accountRow) (bool, error) {
	if on, ok := pass.consent[acct.userID]; ok {
		return on, nil
	}
	if acct.consent == 0 && s.now().Sub(time.Unix(0, acct.consentChecked)) < consentOffReuse {
		pass.consent[acct.userID] = false
		return false, nil
	}
	on, err := s.readConsent(ctx, acct)
	if err != nil {
		return false, err
	}
	pass.consent[acct.userID] = on
	return on, nil
}

package identity

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("identity_grants", 6, `ALTER TABLE identity_grants ADD COLUMN xtream_username TEXT; ALTER TABLE identity_grants ADD COLUMN xtream_password TEXT NOT NULL DEFAULT ''; ALTER TABLE identity_grants ADD COLUMN xtream_stream_limit INTEGER NOT NULL DEFAULT 0; CREATE UNIQUE INDEX identity_grants_xtream_username ON identity_grants(xtream_username);`)
}

var errXtreamNameUnavailable = errors.New("identity: Xtream username unavailable")

func nullableXtreamUsername(name string) any {
	if name == "" {
		return nil
	}
	return name
}

// IssueXtream returns the one-time display pair and its internal grant credential.
// The same grant row owns expiry, device admission, policy and revocation.
func (g *Grants) IssueXtream(ctx context.Context, viewerID int64, device core.Device) (username, password, credential string, err error) {
	return g.IssueAppLogin(ctx, viewerID, "xtream", device)
}

// IssueAppLogin issues a dedicated, revocable app login (a generated username
// and app password) for one surface. Xtream and plugin frontend adapters that
// authenticate with query credentials use it. The password is sealed so the
// owner can reveal it, and so a client that proves it with a salted hash
// token instead of sending it can still be verified. The grant row (columns
// named for Xtream, which introduced them) owns expiry, devices, policy and
// revocation, exactly as for any other grant on that surface.
func (g *Grants) IssueAppLogin(ctx context.Context, viewerID int64, surface string, device core.Device) (username, password, credential string, err error) {
	if g.sealer == nil {
		return "", "", "", errors.New("identity: credential encryption unavailable")
	}
	if surface == "" {
		return "", "", "", errors.New("identity: surface required")
	}
	viewer, err := g.viewers.ActiveUser(ctx, viewerID)
	if err != nil {
		return "", "", "", err
	}
	name := viewer.Title
	if strings.TrimSpace(name) == "" {
		name = viewer.Username
	}
	var base strings.Builder
	for _, c := range strings.ToLower(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			base.WriteRune(c)
			if base.Len() == 10 {
				break
			}
		}
	}
	if base.Len() == 0 {
		base.WriteString("viewer")
	}
	length := frontend.AppLoginPasswordLength(surface)
	// Try random suffixes without replacement. The unique index arbitrates
	// concurrent creations; an available suffix is never missed by bad luck.
	var suffixes [100]int
	for i := range suffixes {
		suffixes[i] = i
	}
	for remaining := 100; remaining > 0; remaining-- {
		suffix, e := rand.Int(rand.Reader, big.NewInt(int64(remaining)))
		if e != nil {
			return "", "", "", e
		}
		index := int(suffix.Int64())
		number := suffixes[index]
		suffixes[index] = suffixes[remaining-1]
		username = fmt.Sprintf("%s%02d", base.String(), number)
		raw := make([]byte, length)
		for i := range raw {
			n, e := rand.Int(rand.Reader, big.NewInt(int64(len(frontend.XtreamPasswordAlphabet))))
			if e != nil {
				return "", "", "", e
			}
			raw[i] = frontend.XtreamPasswordAlphabet[n.Int64()]
		}
		password = frontend.FormatAppLoginPassword(string(raw))
		credential, _ = frontend.AppLoginCredential(username, password)
		sealed, e := g.sealer.SealField(password, "identity_grants", "xtream_password", strconv.FormatInt(viewerID, 10), username)
		if e != nil {
			return "", "", "", e
		}
		_, err = g.issue(ctx, viewerID, nil, surface, device, grantIssue{token: credential, xtreamUsername: username, sealedPassword: sealed})
		if errors.Is(err, errXtreamNameUnavailable) {
			continue
		}
		if err != nil {
			return "", "", "", err
		}
		return username, password, credential, nil
	}
	return "", "", "", errXtreamNameUnavailable
}

// RevealXtream is called only by the owner's explicit reveal endpoint.
func (g *Grants) RevealXtream(ctx context.Context, viewerID int64, hash string) (username, password string, available bool, err error) {
	return g.RevealAppLogin(ctx, viewerID, "xtream", hash)
}

// RevealAppLogin opens one app login's sealed password. It is called only by
// the owner's explicit reveal endpoint.
func (g *Grants) RevealAppLogin(ctx context.Context, viewerID int64, surface, hash string) (username, password string, available bool, err error) {
	var sealed string
	err = g.db.QueryRowContext(ctx, `SELECT COALESCE(xtream_username,''),xtream_password FROM identity_grants WHERE hash=? AND viewer_id=? AND surface=? AND revoked=0 AND expires>?`, hash, viewerID, surface, g.now().UnixMilli()).Scan(&username, &sealed)
	if err != nil || sealed == "" {
		return username, "", false, err
	}
	if g.sealer == nil {
		return "", "", false, errors.New("identity: credential encryption unavailable")
	}
	plain, legacy, err := g.sealer.OpenField(sealed, "identity_grants", "xtream_password", strconv.FormatInt(viewerID, 10), username)
	if err != nil {
		return "", "", false, err
	}
	if legacy {
		return "", "", false, errors.New("identity: unsealed Xtream password")
	}
	return username, plain, true, nil
}

// AppLoginTokenCredential verifies a salted-hash proof of an app login's
// password, md5(password + salt) in lowercase hex, which some protocols send
// on every request instead of the password itself. It returns the login's
// internal grant credential, which the caller resolves like any other, so
// expiry, revocation and viewer state are checked in one place. The password
// is compared in the spellings a person may have typed it: as displayed, and
// without its group dashes, in lower or upper case.
func (g *Grants) AppLoginTokenCredential(ctx context.Context, surface, username, token, salt string) (string, bool) {
	username = strings.ToLower(strings.TrimSpace(username))
	token = strings.ToLower(strings.TrimSpace(token))
	if surface == "" || username == "" || len(username) > 12 || len(token) != 32 || salt == "" || len(salt) > 256 || g.sealer == nil {
		return "", false
	}
	var sealed string
	var viewerID int64
	err := g.db.QueryRowContext(ctx, `SELECT viewer_id,xtream_password FROM identity_grants WHERE xtream_username=? AND surface=? AND revoked=0`, username, surface).Scan(&viewerID, &sealed)
	if err != nil || sealed == "" {
		return "", false
	}
	plain, legacy, err := g.sealer.OpenField(sealed, "identity_grants", "xtream_password", strconv.FormatInt(viewerID, 10), username)
	if err != nil || legacy || plain == "" {
		return "", false
	}
	want, err := hex.DecodeString(token)
	if err != nil {
		return "", false
	}
	bare := strings.ReplaceAll(plain, "-", "")
	matched := 0
	for _, candidate := range []string{plain, bare, strings.ToUpper(plain), strings.ToUpper(bare)} {
		sum := md5.Sum([]byte(candidate + salt))
		matched |= subtle.ConstantTimeCompare(sum[:], want)
	}
	if matched != 1 {
		return "", false
	}
	return frontend.AppLoginCredential(username, plain)
}

func (g *Grants) SetXtreamStreamLimit(ctx context.Context, viewerID int64, hash string, limit int) error {
	if limit < 0 || limit > 1000 {
		return errors.New("identity: invalid stream limit")
	}
	res, err := g.db.ExecContext(ctx, `UPDATE identity_grants SET xtream_stream_limit=? WHERE hash=? AND viewer_id=? AND surface='xtream' AND revoked=0 AND expires>?`, limit, hash, viewerID, g.now().UnixMilli())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return core.ErrUnauthorized
	}
	return nil
}

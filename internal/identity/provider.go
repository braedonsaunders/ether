package identity

import (
	"context"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
)

// Proof carries provider-specific credentials. Providers interpret only the
// fields they support; future providers can add parameters without changing
// identity_links or grants.
type Proof struct {
	Username, Password, Token string
	Parameters                map[string]string
}

// VerifiedIdentity is proof of a linked identity, not a surface grant.
type VerifiedIdentity struct {
	ViewerID, IdentityID int64
	Provider, Subject    string
}

// Provider verifies a proof and resolves it to a household viewer. Grant
// issuance and surface authorization are deliberately separate.
type Provider interface {
	Name() string
	Verify(context.Context, Proof) (VerifiedIdentity, error)
}

func (*Local) Name() string { return "local" }
func (l *Local) Verify(ctx context.Context, proof Proof) (VerifiedIdentity, error) {
	viewer, id, err := l.Authenticate(ctx, proof.Username, proof.Password)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	return VerifiedIdentity{ViewerID: viewer, IdentityID: id, Provider: l.Name(), Subject: proof.Username}, nil
}

// PlexProvider delegates Plex proofs to the existing Plex authenticator,
// which validates membership against the current server.
type PlexProvider struct{ Auth PlexAuthenticator }

func (PlexProvider) Name() string { return "plex" }
func (p PlexProvider) Verify(ctx context.Context, proof Proof) (VerifiedIdentity, error) {
	if p.Auth == nil || core.IsGrantCredential(proof.Token) {
		return VerifiedIdentity{}, core.ErrUnauthorized
	}
	user, err := p.Auth.Authenticate(ctx, proof.Token)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	if user == nil || user.PlexID <= 0 {
		return VerifiedIdentity{}, core.ErrUnauthorized
	}
	return VerifiedIdentity{ViewerID: user.ID, Provider: p.Name(), Subject: strconv.FormatInt(user.PlexID, 10)}, nil
}

var _ Provider = (*Local)(nil)
var _ Provider = PlexProvider{}

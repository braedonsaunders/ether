package identity

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// PlexAuthenticator validates only Plex-surface credentials.
type PlexAuthenticator interface {
	Authenticate(context.Context, string) (*core.User, error)
}

// Resolver routes credentials to their surface-specific validator. Plex tokens
// never fall back to grant validation and grants never reach plex.tv.
type Resolver struct {
	plex   PlexAuthenticator
	grants *Grants
}

func NewResolver(plex PlexAuthenticator, grants *Grants) (*Resolver, error) {
	if plex == nil || grants == nil {
		return nil, errors.New("identity: Plex authenticator and grants required")
	}
	return &Resolver{plex: plex, grants: grants}, nil
}
func (r *Resolver) Resolve(ctx context.Context, surface, credential string) (*core.Viewer, error) {
	if surface == "plex" {
		if IsGrantCredential(credential) {
			return nil, core.ErrUnauthorized
		}
		return r.plex.Authenticate(ctx, credential)
	}
	return r.grants.Resolve(ctx, surface, credential)
}

var _ core.Identity = (*Resolver)(nil)

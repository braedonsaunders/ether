package frontend

import (
	"context"
	"time"
)

// SourceOfferTarget contains one exact scoped selector, never an upstream ID.
// A resolved detail selector also carries its immutable source revision.
type SourceOfferTarget struct {
	Item          RatingKey
	VersionHandle string
	DetailSource  string
	ETag          string
}

type SourceOffer struct {
	ID        string
	ExpiresAt time.Time
	Target    SourceOfferTarget
}

// SourceOffers maps scoped selectors to bounded, expiring numeric protocol IDs.
// IDs are never credentials and must be resolved under current viewer policy.
type SourceOffers interface {
	Issue(context.Context, VersionScope, SourceOfferTarget) (SourceOffer, error)
	IssueBatch(context.Context, VersionScope, []SourceOfferTarget) ([]SourceOffer, error)
	Resolve(context.Context, VersionScope, string) (SourceOffer, error)
	Revoke(context.Context, VersionScope, string) error
}

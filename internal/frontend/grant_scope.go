package frontend

import "context"

// GrantScopes identifies a login independently of its rotating access tokens.
// Scope values are correlation identifiers, never accepted as HTTP credentials.
// Both operations enforce the current viewer/device/surface and grant policy.
type GrantScopes interface {
	GrantScope(context.Context, string, Device) (string, error)
	GrantScopeCurrent(context.Context, string, Device) (*Viewer, error)
}

// AuthenticationFeatures exposes configured identity entry points without
// attempting authentication or issuing a pairing grant.
type AuthenticationFeatures struct{ Password, QuickConnect bool }
type AuthenticationFeatureSource interface{ AuthenticationFeatures() AuthenticationFeatures }

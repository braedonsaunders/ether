package core

import (
	"context"
	"strings"
)

// Viewer is the household member that owns per-viewer state. Its ID is the
// existing local auth_users ID, independent of any identity provider.
type Viewer = User

// Device identifies the client receiving a surface-specific credential.
type Device struct {
	ID         string
	Client     string
	Product    string
	Version    string
	Platform   string
	DeviceName string
	Language   string
}

// Identity resolves a credential only for the supplied surface. Surface IDs
// are protocol-defined strings so additional front ends can register them.
type Identity interface {
	Resolve(ctx context.Context, surface, credential string) (*Viewer, error)
}

// GrantCredentialPrefix reserves Ether-issued credentials from external providers.
const GrantCredentialPrefix = "eg1_"

// IsGrantCredential reports whether a credential uses Ether's reserved prefix.
func IsGrantCredential(s string) bool { return strings.HasPrefix(s, GrantCredentialPrefix) }

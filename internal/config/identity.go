package config

// IdentitySame marks a grab identity that follows the same-media search
// identity. It is valid only for grab values; search values name a preset.
const IdentitySame = "same"

// ClientIdentity is the global policy for how Ether identifies itself to
// Newznab and Torznab indexers, split by request purpose and media. Empty
// values mean the default "ether" identity. A custom identity is one user
// agent and header set used wherever this policy resolves to "custom".
type ClientIdentity struct {
	SearchTV     string            `json:"searchTv,omitempty"`
	SearchMovies string            `json:"searchMovies,omitempty"`
	GrabTV       string            `json:"grabTv,omitempty"`
	GrabMovies   string            `json:"grabMovies,omitempty"`
	UserAgent    string            `json:"userAgent,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
}

// DefaultClientIdentity keeps both grabs following the same-media search.
func DefaultClientIdentity() ClientIdentity {
	return ClientIdentity{GrabTV: IdentitySame, GrabMovies: IdentitySame}
}

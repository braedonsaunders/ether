package config

// Plugins configures the signed plugin catalogue (SPEC WS12). Ether ships
// no third-party catalogue URL; the owner sets IndexURL and PublicKey when
// connecting a catalogue they trust.
type Plugins struct {
	RuntimeLimits    PluginRuntimeLimits            `json:"runtimeLimits,omitempty"`
	RuntimeOverrides map[string]PluginRuntimeLimits `json:"runtimeOverrides,omitempty"`
	// IndexURL is the URL of index.json (with a sibling index.json.sig).
	// Empty disables automatic catalogue fetches.
	IndexURL string `json:"indexUrl,omitempty"`
	// PublicKey is the base64-encoded ed25519 public key that signs index.json.
	PublicKey string `json:"publicKey,omitempty"`
}

// PluginRuntimeLimits are owner-controlled ceilings, not allocations. Zero
// inherits the default (globally) or the global ceiling (per plugin).
type PluginRuntimeLimits struct {
	FetchBytes           int64  `json:"fetchBytes,omitempty"`
	ArchiveMemberBytes   int64  `json:"archiveMemberBytes,omitempty"`
	ArchiveExpandedBytes int64  `json:"archiveExpandedBytes,omitempty"`
	ArchiveMembers       int    `json:"archiveMembers,omitempty"`
	MemoryBytes          uint64 `json:"memoryBytes,omitempty"`
	Deadline             string `json:"deadline,omitempty"`
	UnitBudget           int64  `json:"unitBudget,omitempty"` // zero means effectively unlimited
}

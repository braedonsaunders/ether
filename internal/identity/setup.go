package identity

// ProtocolSetupComplete always reports that protocol-side setup is complete.
// Household setup and Plex claim are available only from the owner console.
func ProtocolSetupComplete() bool { return true }

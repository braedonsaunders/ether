package experience

import "github.com/braedonsaunders/ether/internal/core"

// DownloadsAllowed is shared capability admission; handlers still enforce it.
func DownloadsAllowed(viewer *core.User, ready, allowFriends bool, policy AccessPolicy) bool {
	if viewer == nil || !ready {
		return false
	}
	if viewer.IsOwner {
		return true
	}
	if p, ok := policy.(core.DownloadAccessPolicy); ok {
		return p.AllowDownloads(viewer)
	}
	return allowFriends
}

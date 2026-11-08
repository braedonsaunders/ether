package plexwatch

import "github.com/braedonsaunders/ether/internal/store"

// Per-viewer lifecycle for this component's tables: how a viewer's rows
// are purged on removal and moved on merge. See store.RegisterViewerTable.
func init() {
	store.RegisterViewerTable("plexwatch", "plexwatch_account", "user_id", store.ViewerMergeMove)
	store.RegisterViewerTable("plexwatch", "plexwatch_fence", "user_id", store.ViewerMergeMove)
	store.RegisterViewerTable("plexwatch", "plexwatch_pin", "user_id", store.ViewerMergeMove)
	// Last-read Plex state is per account; a merged viewer reads it again.
	store.RegisterViewerTable("plexwatch", "plexwatch_remote", "user_id", store.ViewerMergeDrop)
	store.RegisterViewerTable("plexwatch", "plexwatch_expand", "user_id", store.ViewerMergeDrop)
}

// Server-to-server sync. A merged viewer keeps a mapping only where the
// target had none on that server; echo and unmatched rows are rebuilt by
// the next pass (read-before-write keeps that from double-sending).
func init() {
	store.RegisterViewerTable("plexwatch_pms", "plexwatch_pms_viewer", "user_id", store.ViewerMergeMove, "server_id")
	store.RegisterViewerTable("plexwatch_pms", "plexwatch_pms_sent", "user_id", store.ViewerMergeDrop)
	store.RegisterViewerTable("plexwatch_pms", "plexwatch_pms_unmatched", "user_id", store.ViewerMergeDrop)
}

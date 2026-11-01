package main

import (
	"path/filepath"
	"strings"
)

// appTranslocated reports whether macOS is running this copy from App
// Translocation (a read-only temporary mount under /AppTranslocation/).
// That mount is not the folder the owner copied, and it goes away when the
// process exits. Ether must not register launchd against it, and must not
// move or delete either copy: nothing in the tray, updater, or installer
// removes a running .app. A vanished study bundle was the original path
// disappearing out from under a later exec (the binary was already gone,
// logged as "No such file or directory"), which this process cannot undo
// by deleting anything — the guard is that we never do.
func appTranslocated(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/AppTranslocation/")
}

// bundleRoot is the .app directory containing path, or "" when path is not
// inside an app bundle.
func bundleRoot(path string) string {
	clean := filepath.Clean(path)
	for {
		if strings.HasSuffix(strings.ToLower(clean), ".app") {
			return clean
		}
		next := filepath.Dir(clean)
		if next == clean {
			return ""
		}
		clean = next
	}
}

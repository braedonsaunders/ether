package app

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// frontendVersion exposes only facts known for this candidate. Resolver names
// are release labels, not verified file names, and a release's aggregate size
// does not prove a particular file revision or part count.
func frontendVersion(title *core.Title, version core.ReleaseVariant) frontend.Version {
	out := frontend.Version{ID: version.ID, Name: version.Label, Media: version.Info, AddedAt: versionAddedAt(title, version.Release.PubDate)}
	if !version.Release.PubDate.IsZero() {
		modified := version.Release.PubDate
		out.PublishedAt = &modified
	}
	return out
}

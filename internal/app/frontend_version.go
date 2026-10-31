package app

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// frontendVersion exposes only facts known for this candidate. Resolver names
// are release labels, not verified file names, and a release's aggregate size
// does not prove a particular file revision or part count. ID stays the
// candidate id; broker issuance replaces it before a client-facing handle.
func frontendVersion(title *core.Title, version core.ReleaseVariant) frontend.Version {
	offer := experience.VersionOfferFrom(title, version)
	out := frontend.Version{ID: offer.ID, Name: offer.Name, Edition: offer.Edition, Media: offer.Media, AddedAt: offer.AddedAt, PublishedAt: offer.Publication}
	if !offer.Media.Estimated {
		if name := probedFileName(offer.Name); name != "" {
			out.FileName = name
			// The probed basename is that one file, so the version is a single part.
			one := 1
			out.PartCount = &one
		}
	}
	return out
}

package mediabrowser

import "github.com/braedonsaunders/ether/internal/frontend"

// jellyfinItemDetailDTO reports only the host's policy-backed estimate.
func (i *instance) jellyfinItemDetailDTO(detail frontend.ItemDetail, dto map[string]any) {
	if detail.CanPlay == nil {
		return
	}
	if *detail.CanPlay {
		dto["PlayAccess"] = "Full"
	} else {
		dto["PlayAccess"] = "None"
	}
}

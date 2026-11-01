package mediabrowser

import (
	"context"
	"strconv"
)

// Emby's NameLongIdPair uses a numeric ID; Jellyfin uses the Host's GUID.
func (i *instance) namedFacetWireID(ctx context.Context, kind, id string) (any, bool) {
	if i.protocol != "emby" {
		return id, true
	}
	n, err := i.host.IDs.Compact(ctx, "emby", "facet-"+kind, id)
	return n, err == nil && n > 0
}

func (i *instance) resolveNamedFacetWireID(ctx context.Context, kind, id string) (string, error) {
	if i.protocol != "emby" || (kind != "genre" && kind != "studio") {
		return id, nil
	}
	n, err := strconv.ParseInt(id, 10, 32)
	if err != nil {
		// Preserve ordinary GUID-based filters from the facet list routes.
		return id, nil
	}
	if n <= 0 {
		return "", strconv.ErrSyntax
	}
	return i.host.IDs.Resolve(ctx, "emby", "facet-"+kind, int32(n))
}

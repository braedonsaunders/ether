package newznab

import (
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
)

// bindReleases records the client that produced these results. Feed/backend
// fields and display names cannot select a different configured trust context.
func (c *Client) bindReleases(results []core.Release) []core.Release {
	if len(results) == 0 {
		return results
	}
	out := append([]core.Release(nil), results...)
	id := c.indexerID()
	for i := range out {
		out[i].IndexerID = id
	}
	c.markOnDemand(out)
	return out
}

// markOnDemand marks a Stremio add-on's stream links on demand
// (core.Release.StreamOnDemand). Such a link asks the add-on to resolve,
// download or import that release; it is not a plain file, so every open
// costs the add-on (and often its Usenet or debrid account) real work.
func (c *Client) markOnDemand(results []core.Release) {
	if cfg := c.Config(); !cfg.IsStremio() && !cfg.IsAddon() {
		return
	}
	for i := range results {
		if results[i].StreamURL != "" {
			results[i].StreamOnDemand = true
		}
	}
}

// feedReleases also namespaces feed GUIDs: different indexers may use the same
// GUID and display name, including when their NZB disk directory is shared.
func (c *Client) feedReleases(results []core.Release) []core.Release {
	out := c.bindReleases(results)
	for i := range out {
		out[i].ID = releaseID(out[i].IndexerID, out[i].GUID, out[i].NZBURL+out[i].Name)
	}
	return out
}

// releaseClient never infers private trust from a feed URL. An unbound
// record must identify one unique configured name; duplicate names fail closed.
func (m *Manager) releaseClient(r core.Release) (*Client, error) {
	var found *Client
	for _, c := range m.Clients() {
		matches := c.indexerID() == r.IndexerID
		if r.IndexerID == "" {
			matches = c.Name() == r.Indexer
		}
		if !matches {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("newznab: ambiguous originating indexer for release %q", r.Name)
		}
		found = c
	}
	if found == nil {
		return nil, fmt.Errorf("newznab: originating indexer for release %q is no longer configured", r.Name)
	}
	return found, nil
}

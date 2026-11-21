package resolver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"

	"github.com/braedonsaunders/ether/internal/vault"
)

// ProfileFingerprint identifies the quality definition and effective ordered audio preferences.
func ProfileFingerprint(profile core.QualityProfile, audio []string) string {
	data, _ := json.Marshal([]any{profile.SelectionKey, profile.ComputeSelectionKey(), audio})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// The cutoff is the highest configured quality group, or the effective maximum
// resolution without a ladder. Soft minimum format scores still apply.
func reachesVaultCutoff(c candidate, p core.QualityProfile) bool {
	if c.belowScore || c.fallback || c.audioUnverified || c.identityWeak || c.policy.Fallback != 0 || !c.langOK || c.bandwidth < 0 {
		return false
	}
	if len(p.QualityOrder) > 0 {
		return c.keys.position == len(p.QualityOrder)
	}
	return p.MaxHeight > 0 && c.keys.height >= p.MaxHeight
}

func (s *Service) libraryCandidates(ctx context.Context, req core.ResolveRequest, search core.SearchRequest, p core.QualityProfile, fp string, tr *Trace, bypass bool) ([]core.Release, *core.Resolution, bool) {
	if s.vault == nil || channelAvailabilityCheck(ctx) || core.SpeculativeIndexerWork(ctx) || core.IsSpeculative(ctx) || len(req.Exclude) > 0 {
		return nil, nil, false
	}
	entries, err := s.vault.Search(ctx, search)
	if err != nil {
		return nil, nil, false
	}
	var releases []core.Release
	fingerprints := map[vault.Key]string{}
	for _, e := range entries {
		if e.Health == vault.HealthDead {
			continue
		}
		r := vault.ReleaseFor(e)
		r.Private = e.Private
		r.PubDate = time.Time{}
		releases = append(releases, r)
		fingerprints[e.Key] = e.ProfileFP
	}
	releases = s.normalizeSearchResults(search, releases)
	if bypass || core.IndexerPurpose(ctx) == core.IndexerPurposeConsole || len(releases) == 0 {
		return releases, nil, false
	}
	if _, slot := parseVersionSlot(req.Prefer); slot {
		return releases, nil, false
	}
	local := Trace{}
	ranked, _, err := s.rankCandidatesLocal(ctx, req, search, p, releases, &local, "", false)
	if err != nil || len(ranked) == 0 {
		return releases, nil, false
	}
	pickIdx := 0
	if req.Prefer != "" {
		pickIdx = -1
		for i, c := range ranked {
			if c.rel.ID == req.Prefer {
				pickIdx = i
				break
			}
		}
		if pickIdx < 0 {
			return releases, nil, false
		}
	} else if req.PreferredOnly {
		return releases, nil, false
	} else {
		best := ranked[0]
		if !reachesVaultCutoff(best, p) || fingerprints[vault.KeyOf(best.rel)] != fp {
			return releases, nil, false
		}
	}
	play, cancel := context.WithTimeout(ctx, playStartDeadline)
	defer cancel()
	i, payload, err := s.validateCandidates(play, ranked[pickIdx:pickIdx+1], &local, 1)
	if i < 0 || err != nil {
		return slices.DeleteFunc(releases, func(r core.Release) bool { return r.ID == ranked[pickIdx].rel.ID }), nil, true
	}
	tr.Candidates = local.Candidates
	tr.Winner = ranked[pickIdx].rel.ID
	if counter, ok := s.vault.(vault.SearchCounter); ok {
		key := vault.KeyOf(ranked[pickIdx].rel)
		s.persistBookkeeping(ctx, func(ctx context.Context) { _ = counter.SearchAvoided(ctx, key) })
	}
	tr.Discovery = append(tr.Discovery, DiscoveryTrace{Path: "vault", Count: len(releases)})
	return nil, s.resolution(req.Title.Key, ranked, pickIdx, payload, tr), false
}

func (s *Service) verifyVault(ctx context.Context, key vault.Key) error {
	e, err := s.vault.Entry(ctx, key)
	if err != nil {
		return err
	}
	if e.Health == vault.HealthDead {
		return core.ErrReleaseRejected
	}
	if e.Kind == vault.KindTorrent || e.Health == vault.HealthOK && s.now().Sub(e.HealthAt) < time.Hour {
		return nil
	}
	if s.verifier == nil {
		return core.ErrNotAvailable
	}
	ctx, cancel := context.WithTimeout(ctx, playStartDeadline)
	defer cancel()
	r, err := s.vault.Get(ctx, key)
	if err != nil {
		return err
	}
	healthy, err := vault.VerifyRecord(ctx, r, s.verifier)
	if err != nil {
		return err
	}
	if !healthy {
		s.vault.MarkHealth(ctx, key, vault.HealthDead)
		return core.ErrReleaseRejected
	}
	s.vault.MarkHealth(ctx, key, vault.HealthOK)
	return nil
}

func (s *Service) recordChoice(ctx context.Context, r core.Release, fp string, payload []byte) {
	key := vault.KeyOf(r)
	if key != "" {
		if r.Protocol == core.ProtocolTorrent {
			s.vault.Touch(ctx, key, true)
		} // Newznab counts Usenet reads at fetch.
	} else if r.Protocol == core.ProtocolTorrent {
		var files []vault.TorrentFile
		if p, err := debrid.DecodePayload(payload); err == nil {
			r.InfoHash = cmp.Or(p.InfoHash, r.InfoHash)
			r.MagnetURL = cmp.Or(p.Magnet, r.MagnetURL)
			r.Private = r.Private || p.Private
			if p.Path != "" {
				files = []vault.TorrentFile{{Path: p.Path, Size: p.Size}}
			}
		}
		put := vault.TorrentRecord(r, files, nil)
		if put.Torrent == nil || put.Torrent.InfoHash == "" {
			return
		}
		put.ProfileFP = fp
		s.vault.Put(ctx, put)
		return
	} else {
		var found bool
		id := r.ID
		if r.Target != nil && r.Target.PackID != "" {
			id = r.Target.PackID
		}
		key, found, _ = s.vault.ByAlias(ctx, r.IndexerID, r.ID)
		if !found && id != r.ID {
			key, found, _ = s.vault.ByAlias(ctx, r.IndexerID, id)
		}
		if !found {
			key, found, _ = s.vault.ByAlias(ctx, vault.LegacyAliasIndexer, id)
		}
		if !found {
			return
		}
	}
	if writer, ok := s.vault.(vault.Chooser); ok {
		writer.MarkChosen(ctx, key, fp)
	}
}

func (s *Service) markKept(ctx context.Context, page []HistoryRecord) {
	membership, ok := s.vault.(vault.Membership)
	if !ok || len(page) == 0 {
		return
	}
	releases := make([]core.Release, 0, len(page))
	for _, h := range page {
		if h.ReleaseID == "" {
			continue
		}
		r := core.Release{ID: h.ReleaseID, IndexerID: h.indexerID}
		if h.packID != "" {
			r.Target = &core.ReleaseTarget{PackID: h.packID}
		}
		releases = append(releases, r)
	}
	kept, err := membership.Kept(ctx, releases)
	if err != nil {
		return
	}
	for i := range page {
		page[i].Kept = kept[page[i].ReleaseID]
	}
}

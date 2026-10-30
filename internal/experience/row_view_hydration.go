package experience

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	rowViewHydrationChunk  = 200
	rowViewMemberPage      = 100
	rowViewMemberScanLimit = 10000
)

// RowViewCollectionPage is one viewer-scoped page of collection member keys.
// It carries no protocol or wire context.
type RowViewCollectionPage struct {
	Keys  []core.RatingKey
	Total int
}

// RowViewCollectionItems loads one page of a viewer's collection. A nil
// function means membership cannot be established.
type RowViewCollectionItems func(ctx context.Context, viewer *core.User, collection core.RatingKey, offset, limit int) (RowViewCollectionPage, error)

// RowViewEntry resolves one entry the batch path does not own. Callers bind
// device and protocol outside this port. The returned entry is passed through,
// including a handle if the caller set one. This hydrator does not mint one.
type RowViewEntry func(ctx context.Context, ref core.NeutralEntryRef) (frontend.RowEntry, error)

// RowViewGuide returns the viewer's current live guide for a single policy pass.
type RowViewGuide func(ctx context.Context) ([]frontend.RowEntry, error)

// RowViewTitleBatch loads titles by key from the local catalogue.
type RowViewTitleBatch func(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error)

// RowViewTitleRead loads one title, including an existing home-release admission.
type RowViewTitleRead func(ctx context.Context, key core.RatingKey) (*core.Title, error)

// RowViewMembers reports whether every title ref is still a member of the
// viewer-visible collection. The scan is bounded and fails closed when
// membership cannot be established. It keeps no result for a later pass.
func RowViewMembers(ctx context.Context, items RowViewCollectionItems, viewer *core.User, collection core.RatingKey, refs []core.NeutralEntryRef) bool {
	if items == nil || viewer == nil {
		return false
	}
	want := make(map[core.RatingKey]bool)
	for _, ref := range refs {
		if ref.Kind != string(frontend.RowEntryTitle) {
			return false
		}
		key, err := core.ParseRatingKey(ref.ID)
		if err != nil {
			return false
		}
		want[key] = true
	}
	if len(want) == 0 {
		return false
	}
	for offset := 0; offset < rowViewMemberScanLimit; {
		page, err := items(ctx, viewer, collection, offset, rowViewMemberPage)
		if err != nil {
			return false
		}
		for _, key := range page.Keys {
			delete(want, key)
		}
		if len(want) == 0 {
			return true
		}
		if len(page.Keys) == 0 {
			return false
		}
		offset += len(page.Keys)
		if offset >= page.Total {
			return false
		}
	}
	return false
}

// RowViewHydrateLegacy resolves mixed entries and loads the live guide at most
// once in this call. A later call loads the guide again. Source errors are
// returned unchanged, with a nil entry slice. An empty ref list returns a
// non-nil empty slice.
func RowViewHydrateLegacy(ctx context.Context, viewer *core.User, refs []core.NeutralEntryRef, one RowViewEntry, guide RowViewGuide) ([]frontend.RowEntry, error) {
	var liveRows map[string]frontend.RowEntry
	out := make([]frontend.RowEntry, 0, len(refs))
	for _, ref := range refs {
		if ref.Kind != string(frontend.RowEntryLiveChannel) {
			entry, err := one(ctx, ref)
			if err != nil {
				return nil, err
			}
			out = append(out, entry)
			continue
		}
		if liveRows == nil {
			entries, err := guide(ctx)
			if err != nil {
				return nil, err
			}
			liveRows = make(map[string]frontend.RowEntry, len(entries))
			for _, entry := range entries {
				if entry.Kind == frontend.RowEntryLiveChannel && entry.LiveChannel != nil {
					liveRows[entry.LiveChannel.ID] = entry
				}
			}
		}
		entry, ok := liveRows[ref.ID]
		if !ok {
			return nil, core.ErrNotFound
		}
		out = append(out, entry)
	}
	return out, nil
}

// RowViewHydrateEntries rehydrates titles and inherited parents for this pass
// only. A title or parent batch error falls back to RowViewHydrateLegacy for
// the entire original ref list; the guide memo from this call survives that
// fallback and is not shared with any other viewer, device or pass. A
// home-release read failure is not-found and does not fall back. Non-title
// and virtual-season entries use the legacy port. Batch-built title entries
// leave Handle empty. Visibility is copied per chunk so a parent read cannot
// leak into the caller's policy or a later chunk.
func RowViewHydrateEntries(ctx context.Context, viewer *core.User, refs []core.NeutralEntryRef, one RowViewEntry, batch RowViewTitleBatch, read RowViewTitleRead, visibility Visibility, guide RowViewGuide) ([]frontend.RowEntry, error) {
	out := make([]frontend.RowEntry, len(refs))
	var guideRows []frontend.RowEntry
	var guideErr error
	guideLoaded := false
	cached := func(ctx context.Context) ([]frontend.RowEntry, error) {
		if !guideLoaded {
			guideRows, guideErr = guide(ctx)
			guideLoaded = true
		}
		return guideRows, guideErr
	}
	for start := 0; start < len(refs); start += rowViewHydrationChunk {
		part := refs[start:min(start+rowViewHydrationChunk, len(refs))]
		keys := make([]core.RatingKey, 0, len(part))
		for _, ref := range part {
			if ref.Kind == string(frontend.RowEntryTitle) {
				k, e := core.ParseRatingKey(ref.ID)
				if e != nil {
					return nil, core.ErrNotFound
				}
				keys = append(keys, k)
			}
		}
		titles, err := batch(ctx, keys)
		if err != nil {
			return RowViewHydrateLegacy(ctx, viewer, refs, one, cached)
		}
		parents := make([]core.RatingKey, 0, len(keys))
		seen := map[core.RatingKey]bool{}
		for _, key := range keys {
			if t, ok := titles[key]; ok && t.ShowKey != 0 && !seen[t.ShowKey] {
				seen[t.ShowKey] = true
				parents = append(parents, t.ShowKey)
			}
		}
		inherited, err := batch(ctx, parents)
		if err != nil {
			return RowViewHydrateLegacy(ctx, viewer, refs, one, cached)
		}
		vis := visibility
		vis.GetTitle = func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
			t, ok := inherited[key]
			if !ok {
				return nil, core.ErrNotFound
			}
			if t.Type == core.TypeMovie && t.HomeReleaseChecked && t.HomeReleased.IsZero() && !t.HomeReleaseFallbackAdmitted {
				return read(ctx, key)
			}
			return &t, nil
		}
		var otherRefs []core.NeutralEntryRef
		var otherIndexes []int
		for i, ref := range part {
			if ref.Kind != string(frontend.RowEntryTitle) {
				otherRefs = append(otherRefs, ref)
				otherIndexes = append(otherIndexes, i)
				continue
			}
			key, _ := core.ParseRatingKey(ref.ID)
			t, ok := titles[key]
			if !ok && key.Kind() == core.KindSeason {
				otherRefs = append(otherRefs, ref)
				otherIndexes = append(otherIndexes, i)
				continue
			}
			if !ok || t.Key != key {
				return nil, core.ErrNotFound
			}
			// CachedGet may persist an eligible home-release admission. Preserve that
			// existing behavior instead of exposing the batch's unpersisted decision.
			if t.Type == core.TypeMovie && t.HomeReleaseChecked && t.HomeReleased.IsZero() && !t.HomeReleaseFallbackAdmitted {
				resolved, e := read(ctx, key)
				if e != nil || resolved == nil {
					return nil, core.ErrNotFound
				}
				t = *resolved
			}
			if !vis.Visible(ctx, viewer, &t) {
				return nil, core.ErrNotFound
			}
			copy := t
			out[start+i] = frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &copy}
		}
		if len(otherRefs) > 0 {
			entries, e := RowViewHydrateLegacy(ctx, viewer, otherRefs, one, cached)
			if e != nil {
				return nil, e
			}
			for i, entry := range entries {
				out[start+otherIndexes[i]] = entry
			}
		}
	}
	return out, nil
}

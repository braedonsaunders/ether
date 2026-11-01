// Package experience contains protocol-independent viewer experience logic.
package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// AccessPolicy adds owner-defined restrictions to the mandatory title guard.
type AccessPolicy interface {
	Allowed(*core.User, *core.Title) bool
}

// TitleAllowed applies the mandatory content guards and the owner's policy.
func TitleAllowed(policy AccessPolicy, viewer *core.User, title *core.Title) bool {
	if title == nil || title.Adult {
		return false
	}
	if viewer != nil && contentrating.Exceeds(title.ContentRating, viewer.MaxContentRating) {
		return false
	}
	return policy == nil || policy.Allowed(viewer, title)
}

// SectionAllowed applies a viewer's library restrictions to an existing section.
// An empty restriction list means all sections, as it does for an owner.
func SectionAllowed(viewer *core.User, id int) bool {
	if viewer == nil || viewer.IsOwner || len(viewer.AllowedSections) == 0 {
		return true
	}
	for _, allowed := range viewer.AllowedSections {
		if allowed == id {
			return true
		}
	}
	return false
}

// Visibility applies content, library and inherited show restrictions. The
// section lookup returns false for media types with no configured library.
type Visibility struct {
	Policy          AccessPolicy
	SectionFor      func(core.MediaType) (int, bool)
	GetTitle        func(context.Context, core.RatingKey) (*core.Title, error)
	FilterAvailable func(context.Context, []core.Title) []core.Title
	Sessions        func() []*core.PlaySession
}

func (v Visibility) TitleAllowed(viewer *core.User, title *core.Title) bool {
	if !TitleAllowed(v.Policy, viewer, title) {
		return false
	}
	if v.SectionFor == nil {
		return true
	}
	id, ok := v.SectionFor(title.Type)
	return !ok || SectionAllowed(viewer, id)
}

// Playable checks inherited show policy without requiring discovery evidence.
func (v Visibility) Playable(ctx context.Context, viewer *core.User, title *core.Title) bool {
	if !v.TitleAllowed(viewer, title) {
		return false
	}
	if title.ShowKey == 0 {
		return true
	}
	if v.GetTitle == nil {
		return false
	}
	show, err := v.GetTitle(ctx, title.ShowKey)
	return err == nil && v.TitleAllowed(viewer, show)
}

// Visible includes availability in addition to the playback policy.
func (v Visibility) Visible(ctx context.Context, viewer *core.User, title *core.Title) bool {
	if !v.TitleAllowed(viewer, title) {
		return false
	}
	if len(v.Available(ctx, viewer, []core.Title{*title})) == 0 {
		return false
	}
	return v.Playable(ctx, viewer, title)
}

// Available filters discovery evidence, retaining a viewer's open sources.
// Callers must separately enforce title policy.
func (v Visibility) Available(ctx context.Context, viewer *core.User, titles []core.Title) []core.Title {
	if v.FilterAvailable == nil {
		return titles
	}
	filtered := v.FilterAvailable(ctx, titles)
	if len(filtered) == len(titles) || viewer == nil || v.Sessions == nil {
		return filtered
	}
	keep := map[core.RatingKey]bool{}
	for _, title := range filtered {
		keep[title.Key] = true
	}
	for _, session := range v.Sessions() {
		if session != nil && session.UserID == viewer.ID {
			keep[session.Key] = true
		}
	}
	out := make([]core.Title, 0, len(titles))
	for _, title := range titles {
		if keep[title.Key] {
			out = append(out, title)
		}
	}
	return out
}

// CollectionAllowed checks the library restriction for a collection.
func (v Visibility) CollectionAllowed(viewer *core.User, c *core.Collection) bool {
	if c.Type != core.TypeMovie && c.Type != core.TypeShow {
		return true
	}
	if v.SectionFor == nil {
		return true
	}
	id, ok := v.SectionFor(c.Type)
	return !ok || SectionAllowed(viewer, id)
}

// UpcomingItems retains the existing placeholder discovery rules for a page.
func (v Visibility) UpcomingItems(ctx context.Context, viewer *core.User, items []core.Title, now func() time.Time) []core.Title {
	out := make([]core.Title, 0, len(items))
	for _, title := range items {
		if !v.Playable(ctx, viewer, &title) || title.Released.IsZero() {
			continue
		}
		if !title.Released.After(now()) && len(v.Available(ctx, viewer, []core.Title{title})) == 0 {
			continue
		}
		out = append(out, title)
	}
	return out
}

// TitlesAllowed filters the title/section guard without discovery filtering.
func (v Visibility) TitlesAllowed(viewer *core.User, titles []core.Title) []core.Title {
	out := make([]core.Title, 0, len(titles))
	for i := range titles {
		if v.TitleAllowed(viewer, &titles[i]) {
			out = append(out, titles[i])
		}
	}
	return out
}

// LibrarySection maps a title or episode to the configured library of that kind.
// Seasons and episodes belong to the show library. Unknown kinds use the movie library.
func LibrarySection(libraries []Library, kind core.MediaType) (int, bool) {
	want := core.TypeMovie
	switch kind {
	case core.TypeShow, core.TypeSeason, core.TypeEpisode:
		want = core.TypeShow
	}
	for _, library := range libraries {
		if library.Type == want {
			return library.ID, true
		}
	}
	return 0, false
}

// BindVisibility is the shared library, policy and availability port.
// Frontends supply sources; they do not decide which library a media type uses.
func BindVisibility(policy AccessPolicy, get func(context.Context, core.RatingKey) (*core.Title, error), available func(context.Context, []core.Title) []core.Title, sessions func() []*core.PlaySession, libraries []Library) Visibility {
	return Visibility{Policy: policy, GetTitle: get, FilterAvailable: available, Sessions: sessions,
		SectionFor: func(kind core.MediaType) (int, bool) { return LibrarySection(libraries, kind) }}
}

// MaySetViewerRating is the owner-console rule for changing a household rating cap.
// Delegated sessions and non-owners cannot write it.
func MaySetViewerRating(actor *core.Viewer, active *core.User) bool {
	return actor != nil && !actor.Delegated && active != nil && active.IsOwner
}

// ViewerRestricted chooses inherited visibility for limited or unknown policies.
func ViewerRestricted(policy AccessPolicy, u *core.User) bool {
	if u == nil {
		return true
	}
	if u.IsOwner {
		return false
	}
	if u.MaxContentRating != "" || len(u.AllowedSections) > 0 {
		return true
	}
	if policy == nil {
		return false
	}
	if p, ok := policy.(core.ArtworkAccessPolicy); ok {
		return p.Restricted(u)
	}
	return true
}

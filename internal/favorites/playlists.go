package favorites

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// Playlists projects the marked Favorites playlist from the authoritative set.
// The underlying playlist contains no duplicated favorite item list.
type Playlists struct {
	Base      core.Playlists
	Favorites *Service
	Marked    core.MarkedPlaylists
}

func NewPlaylists(base core.Playlists, favorites *Service) (*Playlists, error) {
	marked, ok := base.(core.MarkedPlaylists)
	if base == nil || favorites == nil || !ok {
		return nil, errors.New("favorites: missing marked playlist support")
	}
	return &Playlists{base, favorites, marked}, nil
}
func (p *Playlists) favorite(ctx context.Context, uid int64) (*core.Playlist, error) {
	return p.Marked.MarkedPlaylist(ctx, uid, "favorites")
}
func (p *Playlists) project(ctx context.Context, uid int64, pl *core.Playlist) (*core.Playlist, error) {
	entries, err := p.Favorites.List(ctx, &core.User{ID: uid}, KindTitle, 0)
	if err != nil {
		return nil, err
	}
	cp := *pl
	cp.Items = make([]core.RatingKey, 0, len(entries))
	updated, err := p.Favorites.UpdatedAt(ctx, uid)
	if err != nil {
		return nil, err
	}
	if updated.After(cp.UpdatedAt) {
		cp.UpdatedAt = updated
	}
	for _, e := range entries {
		k, err := core.ParseRatingKey(e.Subject.ID)
		if err != nil {
			return nil, err
		}
		cp.Items = append(cp.Items, k)
	}
	return &cp, nil
}
func (p *Playlists) List(ctx context.Context, uid int64) ([]core.Playlist, error) {
	items, err := p.Base.List(ctx, uid)
	if err != nil {
		return nil, err
	}
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Key == fav.Key {
			projected, e := p.project(ctx, uid, &items[i])
			if e != nil {
				return nil, e
			}
			items[i] = *projected
		}
	}
	return items, nil
}
func (p *Playlists) Get(ctx context.Context, uid int64, key core.RatingKey) (*core.Playlist, error) {
	pl, err := p.Base.Get(ctx, uid, key)
	if err != nil {
		return nil, err
	}
	fav, e := p.favorite(ctx, uid)
	if e != nil && !errors.Is(e, core.ErrNotFound) {
		return nil, e
	}
	if e == nil && fav.Key == key {
		return p.project(ctx, uid, pl)
	}
	return pl, nil
}
func (p *Playlists) Create(ctx context.Context, uid int64, title string, items []core.RatingKey) (*core.Playlist, error) {
	return p.Base.Create(ctx, uid, title, items)
}
func (p *Playlists) Add(ctx context.Context, uid int64, key core.RatingKey, items []core.RatingKey) error {
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return p.Base.Add(ctx, uid, key, items)
	}
	if err != nil {
		return err
	}
	if fav.Key != key {
		return p.Base.Add(ctx, uid, key, items)
	}
	subjects := make([]Subject, len(items))
	for i, k := range items {
		subjects[i] = Subject{Kind: KindTitle, ID: k.String()}
	}
	_, err = p.Favorites.setMany(ctx, &core.User{ID: uid}, subjects, true, key)
	return err
}
func (p *Playlists) Remove(ctx context.Context, uid int64, key core.RatingKey, index int) error {
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return p.Base.Remove(ctx, uid, key, index)
	}
	if err != nil {
		return err
	}
	if fav.Key != key {
		return p.Base.Remove(ctx, uid, key, index)
	}
	return p.Favorites.removeTitleAt(ctx, &core.User{ID: uid}, key, index)
}
func (p *Playlists) Move(ctx context.Context, uid int64, key core.RatingKey, from, to int) error {
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return p.Base.Move(ctx, uid, key, from, to)
	}
	if err != nil {
		return err
	}
	if fav.Key == key {
		return p.Favorites.MoveTitle(ctx, &core.User{ID: uid}, key, from, to)
	}
	return p.Base.Move(ctx, uid, key, from, to)
}
func (p *Playlists) Delete(ctx context.Context, uid int64, key core.RatingKey) error {
	fav, err := p.favorite(ctx, uid)
	if errors.Is(err, core.ErrNotFound) {
		return p.Base.Delete(ctx, uid, key)
	}
	if err != nil {
		return err
	}
	if fav.Key != key {
		return p.Base.Delete(ctx, uid, key)
	}
	return p.Favorites.setMirror(ctx, &core.User{ID: uid}, false, key)
}

var _ core.Playlists = (*Playlists)(nil)

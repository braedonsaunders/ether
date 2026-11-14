package app

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/plugins"
	"strings"
	"time"
)

func (a *App) catalogueSourceType(ctx context.Context, id string) (string, error) {
	if id == "tmdb-movies" {
		return "movie", nil
	}
	if id == "tmdb-tv" {
		return "show", nil
	}
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 || a.pluginsRT == nil || a.pluginsRT.mgr == nil {
		return "", fmt.Errorf("unknown catalogue source")
	}
	rec, err := a.pluginsRT.mgr.Get(ctx, parts[1])
	if err != nil || !rec.Enabled {
		return "", fmt.Errorf("catalogue source is unavailable")
	}
	if parts[0] == "plugin" {
		if e := rec.Manifest.EffectiveEther(); e != nil {
			for _, k := range e.Contributes.MediaKinds {
				if k.Key == parts[2] {
					return "plugin:" + id, nil
				}
			}
		}
	}
	if parts[0] == "stremio" && rec.Manifest.HasCapability(plugins.CapListSource) {
		for _, cat := range rec.Manifest.Stremio().Catalogs {
			if cat.Standalone() && plugins.StremioCatalogKey(cat) == parts[2] {
				if cat.Type == "movie" {
					return "movie", nil
				}
				if cat.Type == "series" {
					return "show", nil
				}
			}
		}
	}
	return "", fmt.Errorf("unknown catalogue source")
}
func (a *App) validateCatalogueSources(ctx context.Context, lib core.Library, ids []string) error {
	if len(ids) == 0 || len(ids) > 32 {
		return fmt.Errorf("choose between one and 32 catalogue sources")
	}
	if lib.Source == core.LibraryCollection {
		return fmt.Errorf("change the collection source from library settings")
	}
	if lib.Source == core.LibraryBuiltin {
		return fmt.Errorf("built-in catalogue sources cannot be changed")
	}
	typ := lib.MediaType
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("duplicate catalogue source")
		}
		seen[id] = true
		sourceType, err := a.catalogueSourceType(ctx, id)
		if err != nil {
			return err
		}
		if lib.Source == core.LibraryPlugin {
			if len(ids) != 1 || id != "plugin:"+lib.PluginID+":"+lib.KindKey {
				return fmt.Errorf("plugin libraries use their own content kind")
			}
		} else if sourceType != string(typ) {
			return fmt.Errorf("choose sources with the same media type")
		}
	}
	return nil
}

// One loader per App; lock only remote refreshes. The persisted timestamp
// avoids network work after the first bounded load for an opened catalogue.
func (a *App) wireCatalogueSources() {
	if a.catalog == nil {
		return
	}
	lock := make(chan struct{}, 1)
	type sourceFailure struct {
		at  time.Time
		err error
	}
	failures := map[string]sourceFailure{}
	a.catalog.SetCatalogueSourceLoader(func(ctx context.Context, ids []string) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for _, id := range ids {
			if !strings.HasPrefix(id, "stremio:") {
				continue
			}
			typ, err := a.catalogueSourceType(ctx, id)
			if err != nil {
				return err
			}
			var refreshed int64
			_ = a.db.QueryRowContext(ctx, `SELECT refreshed FROM libraries_catalogue_state WHERE source_id=?`, id).Scan(&refreshed)
			if time.Since(time.Unix(refreshed, 0)) < time.Hour {
				continue
			}
			if err := func() error {
				select {
				case lock <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
				defer func() { <-lock }()
				if failed, ok := failures[id]; ok && time.Since(failed.at) < time.Minute {
					return failed.err
				}
				_ = a.db.QueryRowContext(ctx, `SELECT refreshed FROM libraries_catalogue_state WHERE source_id=?`, id).Scan(&refreshed)
				if time.Since(time.Unix(refreshed, 0)) < time.Hour {
					return nil
				}
				if a.collections == nil {
					return fmt.Errorf("catalogue sources unavailable")
				}
				parts := strings.SplitN(id, ":", 3)
				bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				titles, err := a.collections.GatherCatalogueSource(bounded, extend.PluginID(parts[1], parts[2]), typ)
				if err != nil {
					failures[id] = sourceFailure{at: time.Now(), err: err}
					return err
				}
				delete(failures, id)
				unique := make([]core.Title, 0, len(titles))
				seen := map[core.RatingKey]bool{}
				for _, title := range titles {
					if title.Type == core.MediaType(typ) && title.IDs.TMDB > 0 && !seen[title.Key] {
						seen[title.Key] = true
						unique = append(unique, title)
					}
				}
				titles = unique
				if err := a.catalog.UpsertCollectionTitles(bounded, titles); err != nil {
					return err
				}
				tx, err := a.db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				if _, err = tx.ExecContext(ctx, `DELETE FROM libraries_catalogue_members WHERE source_id=?`, id); err != nil {
					return err
				}
				for _, title := range titles {
					if _, err = tx.ExecContext(ctx, `INSERT INTO libraries_catalogue_members(source_id,key) VALUES(?,?) ON CONFLICT DO NOTHING`, id, int64(title.Key)); err != nil {
						return err
					}
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO libraries_catalogue_state(source_id,refreshed,item_count) VALUES(?,?,?) ON CONFLICT(source_id) DO UPDATE SET refreshed=excluded.refreshed,item_count=excluded.item_count,last_error=''`, id, time.Now().Unix(), len(titles)); err != nil {
					return err
				}
				if err = tx.Commit(); err == nil {
					a.catalog.CatalogueSourcesChanged()
				}
				return err
			}(); err != nil {
				_, _ = a.db.ExecContext(ctx, `INSERT INTO libraries_catalogue_state(source_id,refreshed,item_count,last_error) VALUES(?,0,0,?) ON CONFLICT(source_id) DO UPDATE SET last_error=excluded.last_error`, id, err.Error())
				return err
			}
		}
		return nil
	})
}

package app

import (
	"context"
	"fmt"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func (a *App) buildCatalogue(ctx context.Context, cfg config.Config) error {
	log := a.log
	var err error
	// Catalogue.
	a.tmdb = tmdb.NewFromConfig(cfg.Catalog, log.With("component", "tmdb"), parityTMDBOptions()...)
	parityConfigureTMDB(a.tmdb)
	a.closers = append(a.closers, a.tmdb.Close)
	if a.catalog, err = catalog.New(a.db, a.tmdb, cfg.Catalog, log.With("component", "catalog"), append(a.catalogOptions(), parityCatalogueOptions()...)...); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}

	return nil
}

//go:build !plexparity

package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"time"
)

// These hooks are inert in production builds. The plexparity build tag wires
// deterministic dependencies only into disposable differential-test servers.
func parityClock() func() time.Time            { return nil }
func parityCatalogueOptions() []catalog.Option { return nil }
func parityTMDBOptions() []tmdb.Option         { return nil }

func parityCollectionOptions() []collections.Option { return nil }

func parityWarmPersonalization(context.Context, *App) error { return nil }

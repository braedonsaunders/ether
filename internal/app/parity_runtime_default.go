//go:build !plexparity

package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"time"
)

// These hooks are inert in production builds. The plexparity build tag wires
// deterministic dependencies only into disposable differential-test servers.
func parityClock() func() time.Time                  { return nil }
func parityRefreshEvery() time.Duration              { return 0 }
func parityRankerOptions() personalize.RankerOptions { return personalize.RankerOptions{} }
func parityCatalogueOptions() []catalog.Option       { return nil }
func parityTMDBOptions() []tmdb.Option               { return nil }

func parityCollectionOptions() []collections.Option { return nil }

func parityWarmPersonalization(context.Context, *App) error { return nil }

func parityPlexTVBaseURL() string { return "" }

func parityRunCatalogueWorkers() bool { return true }

func parityRunRegistrar() bool { return true }

func parityConfigureTMDB(*tmdb.Client) {}

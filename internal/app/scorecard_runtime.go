//go:build scorecard && !plexparity

package app

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"os"
	"time"
)

func parityClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
}

// The harness explicitly warms each real viewer model before listening.
// Avoid a second scheduler-triggered rebuild racing that initial warmup.
func parityRefreshEvery() time.Duration { return -1 }

// Preserve the default learned-ranker training schedule while the model
// refresh scheduler is disabled. Inference and serve logging remain enabled.
func parityRankerOptions() personalize.RankerOptions {
	return personalize.RankerOptions{TrainEvery: 6 * time.Hour}
}
func parityCatalogueOptions() []catalog.Option {
	return []catalog.Option{catalog.WithNow(parityClock())}
}
func parityTMDBOptions() []tmdb.Option {
	u := os.Getenv("ETHER_PARITY_TMDB_URL")
	if u == "" {
		panic("plexparity build requires ETHER_PARITY_TMDB_URL")
	}
	return []tmdb.Option{tmdb.WithBaseURL(u), tmdb.WithMaxRetries(0)}
}

func parityCollectionOptions() []collections.Option {
	return []collections.Option{collections.WithNow(parityClock())}
}

// Warm every real viewer model before the listener accepts fixture requests.
func parityWarmPersonalization(ctx context.Context, a *App) error {
	users, err := a.users.Users(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := a.personalize.Refresh(ctx, &u); err != nil {
			return fmt.Errorf("fixture warm %s: %w", u.Username, err)
		}
	}
	return nil
}

func parityPlexTVBaseURL() string { return os.Getenv("ETHER_PARITY_PLEXTV_URL") }

// Fixture catalogue is pre-seeded; asynchronous refresh would race requests.
func parityRunCatalogueWorkers() bool { return false }

// Memberships are seeded explicitly; periodic Plex.tv sync would race probes.
func parityRunRegistrar() bool { return false }

func parityConfigureTMDB(c *tmdb.Client) {
	cfg := &tmdb.Configuration{}
	cfg.Images.BaseURL = os.Getenv("ETHER_PARITY_TMDB_URL") + "/images/"
	cfg.Images.SecureBaseURL = cfg.Images.BaseURL
	c.SetConfiguration(cfg)
}

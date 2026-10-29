//go:build plexparity

package app

import (
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/tmdb"
	"os"
	"time"
)

func parityClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
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

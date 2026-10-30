package availability

import (
	"fmt"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

func TestEncryptedObservationSyncPreservesPolicyEpoch(t *testing.T) {
	x := setup(t)
	nonce := 0
	x.s.d.Seal = func(raw string) (string, error) { nonce++; return fmt.Sprintf("%d:%s", nonce, raw), nil }
	x.s.d.Open = func(raw string) (string, error) { return strings.SplitN(raw, ":", 2)[1], nil }
	title := x.cat.titles[core.MovieKey(1)]
	release := x.release("repeat")
	release.Name = "Film.2020.1080p.WEB-DL.ENG"
	write := func() {
		t.Helper()
		if err := x.s.remember(t.Context(), x.f.sources[0], title, release, x.s.policy()); err != nil {
			t.Fatal(err)
		}
		if err := x.s.retainFeed(t.Context(), x.f.sources[0], []core.Release{release}); err != nil {
			t.Fatal(err)
		}
	}
	write()
	before, err := viewerpolicy.Read(t.Context(), x.s.db)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		write()
	}
	after, err := viewerpolicy.Read(t.Context(), x.s.db)
	if err != nil || before != after {
		t.Fatalf("encrypted sync epoch %d -> %d %v", before, after, err)
	}
	release.Name = "Film.2020.1080p.CAM"
	write()
	after, err = viewerpolicy.Read(t.Context(), x.s.db)
	if err != nil || after <= before {
		t.Fatalf("changed descriptor epoch %d -> %d %v", before, after, err)
	}
}

func TestObservationDigestMigrationPreservesExistingRows(t *testing.T) {
	x := setup(t)
	title := x.cat.titles[core.MovieKey(1)]
	release := x.release("upgrade")
	release.Name = "Film.2020.1080p.WEB-DL.ENG"
	if err := x.s.remember(t.Context(), x.f.sources[0], title, release, x.s.policy()); err != nil {
		t.Fatal(err)
	}
	if err := x.s.retainFeed(t.Context(), x.f.sources[0], []core.Release{release}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"availability_releases", "availability_feed_items"} {
		if _, err := x.s.db.Exec("ALTER TABLE " + table + " DROP COLUMN policy_digest"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.s.db.Exec(`DELETE FROM schema_migrations WHERE component='availability' AND version=4`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context(), x.s.db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"availability_releases", "availability_feed_items"} {
		var count int
		if err := x.s.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE policy_digest='' AND payload<>''").Scan(&count); err != nil || count != 1 {
			t.Fatalf("existing %s rows=%d %v", table, count, err)
		}
	}
	if err := x.s.remember(t.Context(), x.f.sources[0], title, release, x.s.policy()); err != nil {
		t.Fatal(err)
	}
	var digest string
	if err := x.s.db.QueryRow(`SELECT policy_digest FROM availability_releases`).Scan(&digest); err != nil || digest == "" {
		t.Fatalf("first ordinary rewrite digest=%q %v", digest, err)
	}
}

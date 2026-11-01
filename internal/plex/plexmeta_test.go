package plex

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/plexmeta"
	"github.com/braedonsaunders/ether/internal/store"
)

const plexMetaGUID = "plex://movie/5d7768265af944001f1f6976"

func plexMetaFixture(t *testing.T, use bool, rt http.RoundTripper) (*fixture, *plexmeta.Service) {
	t.Helper()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO plexguid_mappings(rating_key,guid,checked_at) VALUES(?,?,?)`, int64(fightClub.Key), plexMetaGUID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	pg := plexguid.New(db, plexguid.Options{})
	t.Cleanup(pg.Close)
	client := &http.Client{Transport: http.DefaultTransport}
	if rt != nil {
		client.Transport = rt
	}
	meta := plexmeta.New(db, plexmeta.Options{
		HTTP:             client,
		BaseURL:          "https://metadata.provider.plex.tv",
		Token:            func() string { return "fixture" },
		ClientIdentifier: func() string { return "install-id" },
		GUID:             func(ctx context.Context, k core.RatingKey) string { return pg.Cached(ctx, k) },
		Enabled:          func() bool { return use },
		Background:       true,
	})
	t.Cleanup(meta.Close)
	f := newFixture(func(d *Deps) {
		d.PlexGUID = pg
		d.PlexMeta = meta
		d.DB = db
		d.Config = func() config.Config {
			c := config.Config{}
			c.Server.Port = 32400
			c.Server.FriendlyName = "Ether Test"
			c.Server.MachineIdentifier = "0123456789abcdef0123456789abcdef01234567"
			c.Plex.Claimed = true
			c.Plex.UsePlexMetadata = use
			c.Plex.OwnerUsername = "owner"
			return c
		}
	})
	t.Cleanup(func() { _ = f.srv.Close() })
	return f, meta
}

type plexMetaRoundTrip struct {
	calls atomic.Int32
	block chan struct{}
	body  string
}

func (p *plexMetaRoundTrip) RoundTrip(*http.Request) (*http.Response, error) {
	p.calls.Add(1)
	if p.block != nil {
		<-p.block
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(p.body)),
	}, nil
}

func TestPlexMetaOffIsByteIdentical(t *testing.T) {
	rt := &plexMetaRoundTrip{body: `{"MediaContainer":{"Metadata":[]}}`}
	f, meta := plexMetaFixture(t, false, rt)
	want := f.getWarm(t, "/library/metadata/"+fightClub.Key.String(), jsonAccept...)
	if err := meta.StoreForTest(fightClub.Key, plexMetaGUID, plexmeta.Overlay{Summary: "Plex summary", Studio: "Plex Studio"}); err != nil {
		t.Fatal(err)
	}
	got := f.get(t, "/library/metadata/"+fightClub.Key.String(), jsonAccept...)
	if want.Body.String() != got.Body.String() {
		t.Fatalf("toggle off applied cached Plex fields\nwant %s\ngot %s", want.Body.String(), got.Body.String())
	}
	if rt.calls.Load() != 0 {
		t.Fatal("toggle off fetched Plex")
	}
}

func TestPlexMetaOnServesPlexFieldsOnlyOnPlex(t *testing.T) {
	f, meta := plexMetaFixture(t, true, &plexMetaRoundTrip{body: `{}`})
	if err := meta.StoreForTest(fightClub.Key, plexMetaGUID, plexmeta.Overlay{
		Summary: "Plex summary", Tagline: "Plex tagline", Studio: "Plex Studio",
		ContentRating: "NC-17", Year: 2001, AvailableAt: time.Date(2001, 2, 3, 0, 0, 0, 0, time.UTC),
		AudienceRating: 9.2,
	}); err != nil {
		t.Fatal(err)
	}
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String(), jsonAccept...)
	body := rec.Body.String()
	if !strings.Contains(body, `"summary":"Plex summary"`) || !strings.Contains(body, `"studio":"Plex Studio"`) {
		t.Fatalf("Plex fields missing: %s", body)
	}
	if !strings.Contains(body, `"originallyAvailableAt":"2001-02-03"`) {
		t.Fatalf("Plex date missing: %s", body)
	}
	if !strings.Contains(body, `"addedAt":`+strconv.FormatInt(fightClub.Released.Unix(), 10)) {
		t.Fatalf("addedAt is not the catalogue release date: %s", body)
	}
	if strings.Contains(body, `"addedAt":`+strconv.FormatInt(time.Date(2001, 2, 3, 0, 0, 0, 0, time.UTC).Unix(), 10)) {
		t.Fatal("Plex date changed addedAt")
	}
	catalog := f.catalog.titles[fightClub.Key]
	if catalog.Summary != fightClub.Summary {
		t.Fatalf("catalogue mutated: %q", catalog.Summary)
	}
}

func TestPlexMetaRequestPathDoesNotWait(t *testing.T) {
	block := make(chan struct{})
	rt := &plexMetaRoundTrip{block: block, body: `{"MediaContainer":{"Metadata":[]}}`}
	f, _ := plexMetaFixture(t, true, rt)
	done := make(chan struct{})
	go func() {
		f.get(t, "/library/metadata/"+fightClub.Key.String(), jsonAccept...)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(block)
		t.Fatal("request waited on Plex")
	}
	close(block)
}

func TestPlexMetaSaferRatingHidesTitle(t *testing.T) {
	f, meta := plexMetaFixture(t, true, &plexMetaRoundTrip{body: `{}`})
	if err := meta.StoreForTest(fightClub.Key, plexMetaGUID, plexmeta.Overlay{ContentRating: "R"}); err != nil {
		t.Fatal(err)
	}
	f.srv.d.Auth.(*fakeAuth).users["kid"] = core.User{ID: 30, Username: "kid", MaxContentRating: "PG"}
	pg := f.catalog.titles[fightClub.Key]
	pg.ContentRating = "PG"
	f.catalog.titles[fightClub.Key] = pg
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String(), "X-Plex-Token", "kid", "Accept", "application/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("kid saw Plex-R title: %d %s", rec.Code, rec.Body.String())
	}
}

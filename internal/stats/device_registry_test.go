package stats

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestDeviceRegistryRestartNeverReusesID(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	open := func() *sql.DB {
		db, err := store.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	first := New(db, Options{})
	phone := first.Dashboard().SeeDevice("phone", "Old name", "iOS")
	tv := first.Dashboard().SeeDevice("tv", "Living Room", "tvOS")
	if phone.ID < 2 || tv.ID <= phone.ID {
		t.Fatalf("initial IDs: %v %v", phone, tv)
	}
	first.Dashboard().SeeDevice("phone", "New name", "iOS")
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = open()
	defer db.Close()
	second := New(db, Options{})
	defer second.Close(ctx)
	if got := second.Dashboard().SeeDevice("phone", "", ""); got.ID != phone.ID || got.Name != "New name" {
		t.Fatalf("restored phone: %v", got)
	}
	if got := second.Dashboard().SeeDevice("tv", "", ""); got.ID != tv.ID {
		t.Fatalf("restored tv: %v", got)
	}
	laptop := second.Dashboard().SeeDevice("laptop", "Laptop", "Linux")
	if laptop.ID <= tv.ID {
		t.Fatalf("reused historical ID: %v <= %v", laptop, tv)
	}
	// The allocating observation returns only after its identity is durable, even
	// without a graceful close or metadata-update flush on the allocating service.
	third := New(db, Options{})
	defer third.Close(ctx)
	if got := third.Dashboard().SeeDevice("laptop", "", ""); got.ID != laptop.ID {
		t.Fatalf("unflushed identity: %v want %v", got, laptop)
	}
	other := third.Dashboard().SeeDevice("other", "", "")
	if other.ID <= laptop.ID {
		t.Fatalf("counter not durable: %v", other)
	}
}

func TestDeviceRegistrySeedsHistoricalHighWater(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE stats_device_registry_counter(id INTEGER PRIMARY KEY,next_id INTEGER NOT NULL)`,
		`INSERT INTO stats_device_registry_counter VALUES(1,2)`,
		`CREATE TABLE stats_device_registry(id INTEGER PRIMARY KEY,client_identifier TEXT UNIQUE,name TEXT,platform TEXT,created_at INTEGER)`,
		`CREATE TABLE state_history(device_id INTEGER NOT NULL)`,
		`INSERT INTO state_history VALUES(83),(7)`,
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	r := newDeviceRegistry(db, slog.Default())
	defer r.close(context.Background())
	if _, err = r.load(); err != nil {
		t.Fatal(err)
	}
	dev, err := r.allocate(Device{ClientIdentifier: "new", CreatedAt: time.Now().Unix()})
	if err != nil || dev.ID <= 83 {
		t.Fatalf("historical collision: %v %v", dev, err)
	}
}

func TestDeviceRegistryConcurrentServicesShareIdentity(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, b := New(db, Options{}), New(db, Options{})
	defer a.Close(context.Background())
	defer b.Close(context.Background())
	var wg sync.WaitGroup
	results := make(chan Device, 2)
	for _, svc := range []*Service{a, b} {
		wg.Add(1)
		go func(svc *Service) { defer wg.Done(); results <- svc.Dashboard().SeeDevice("shared", "Phone", "iOS") }(svc)
	}
	wg.Wait()
	close(results)
	var id int64
	for device := range results {
		if device.ID < 2 {
			t.Fatalf("missing identity %v", device)
		}
		if id != 0 && id != device.ID {
			t.Fatalf("divergent identity: %d/%d", id, device.ID)
		}
		id = device.ID
	}
	next := a.Dashboard().SeeDevice("fresh", "", "")
	if next.ID <= id {
		t.Fatalf("ID reused: %v", next)
	}
}

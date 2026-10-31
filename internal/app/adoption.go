package app

import (
	"net/http"
	"path/filepath"
	"sync"

	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/migrate"
)

func (a *App) buildAdoption() {
	a.backups = backup.New(a.db, a.cfg.Path(), filepath.Join(a.opts.DataDir, "backups"), 7)
	if a.clustered() {
		// Record which node holds each bundle (and allow a shared backup
		// directory): scheduled backups run on whichever node leads.
		self := a.cluster.Self()
		a.backups.SetCluster(backup.Node{ID: self.ID, Name: self.Name})
	}
	a.backups.SetRestart(a.scheduleRestart)
	a.migration = migrate.New(a.db, a.catalog, migrate.Options{State: a.state, Logger: a.log.With("component", "migrate")})
}

// backgroundGroup joins component loops before App.Close releases SQLite.
type backgroundGroup struct{ sync.WaitGroup }

func (g *backgroundGroup) start(fn func()) { g.Add(1); go func() { defer g.Done(); fn() }() }

func adoptionHandlers(a *App, m map[string]http.Handler) {
	m["adoption/migration"] = a.migration.Handler()
	m["adoption/backups"] = a.backups.Handler()
}

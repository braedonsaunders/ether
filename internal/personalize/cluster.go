package personalize

import (
	"context"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/clusterbus"
)

// Cluster cache coherence. The home layout is cached for the life of the
// engine and viewer models are rebuilt from stored overrides; when another
// node changes either it publishes a hint:
//
//	layout          reload the home layout
//	overrides:<id>  rebuild that viewer's model if this node holds one

// SetBus connects the cluster bus (nil disconnects). A standalone server
// never calls it.
func (e *Engine) SetBus(bus clusterbus.Bus) {
	iv := clusterbus.NewInvalidator(bus, "personalize", e.log, e.applyRemote)
	e.invalMu.Lock()
	old := e.inval
	e.inval = iv
	e.invalMu.Unlock()
	old.Close()
}

func (e *Engine) notify(hint string) {
	e.invalMu.Lock()
	iv := e.inval
	e.invalMu.Unlock()
	iv.Notify(hint)
}

func (e *Engine) applyRemote(ctx context.Context, hints []string, all bool) {
	layout := all
	var rebuild []int64
	for _, h := range hints {
		kind, arg, _ := strings.Cut(h, ":")
		switch kind {
		case "layout":
			layout = true
		case "overrides":
			if id, err := strconv.ParseInt(arg, 10, 64); err == nil {
				rebuild = append(rebuild, id)
			}
		}
	}
	if layout {
		if d, err := e.store.homeLayout(ctx); err == nil {
			e.mu.Lock()
			e.homeLayout = cloneLayout(d)
			e.layoutVer++
			e.mu.Unlock()
		} else {
			e.log.Warn("personalize: reload home layout", "err", err)
		}
	}
	e.mu.Lock()
	if all {
		rebuild = rebuild[:0]
		for id := range e.models {
			rebuild = append(rebuild, id)
		}
	}
	held := rebuild[:0]
	for _, id := range rebuild {
		if _, ok := e.models[id]; ok {
			held = append(held, id)
		}
	}
	e.mu.Unlock()
	for _, id := range held {
		e.request(id)
	}
}

// FavoritesChanged schedules a local rebuild and notifies peers without
// blocking the caller's committed mutation. Bursts coalesce on the normal
// rebuild queue. The caller must not use this after Close.
func (e *Engine) FavoritesChanged(userID int64) {
	e.scheduleRebuild(userID)
	go e.notify("overrides:" + strconv.FormatInt(userID, 10))
}

package plugins

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plugins/wasm"
	"net/http"
	"time"
)

type runtimeCeilings struct {
	memory                  uint64
	deadline                time.Duration
	units                   int64
	fetch, member, expanded int64
	members                 int
}

func defaultRuntimeCeilings() runtimeCeilings {
	return runtimeCeilings{memory: 2 << 30, deadline: 12 * time.Hour, units: 1 << 60, fetch: 8 << 30, member: 8 << 30, expanded: 32 << 30, members: 65535}
}
func applyRuntimeCeilings(c runtimeCeilings, l config.PluginRuntimeLimits) runtimeCeilings {
	if l.MemoryBytes > 0 {
		c.memory = min(l.MemoryBytes, uint64(wasm.MaxMemoryBytes))
	}
	if d, err := time.ParseDuration(l.Deadline); err == nil && d > 0 {
		c.deadline = min(d, 365*24*time.Hour)
	}
	if l.UnitBudget > 0 {
		c.units = l.UnitBudget
	}
	if l.FetchBytes > 0 {
		c.fetch = min(l.FetchBytes, int64(1<<60))
	}
	if l.ArchiveMemberBytes > 0 {
		c.member = min(l.ArchiveMemberBytes, int64(1<<60))
	}
	if l.ArchiveExpandedBytes > 0 {
		c.expanded = min(l.ArchiveExpandedBytes, int64(1<<60))
	}
	if l.ArchiveMembers > 0 {
		c.members = min(l.ArchiveMembers, 1000000)
	}
	return c
}
func (m *Manager) runtimeConfig() config.Plugins {
	if m.deps.ReadRuntimeConfig != nil {
		return m.deps.ReadRuntimeConfig()
	}
	return config.Plugins{}
}
func (m *Manager) runtimeCeilings(id string) runtimeCeilings {
	cfg := m.runtimeConfig()
	c := applyRuntimeCeilings(defaultRuntimeCeilings(), cfg.RuntimeLimits)
	return applyRuntimeCeilings(c, cfg.RuntimeOverrides[id])
}
func runtimeLimitsWithCeilings(m Manifest, c runtimeCeilings) wasm.Limits {
	l := wasm.Limits{MemoryBytes: min(uint64(32<<20), c.memory), Deadline: c.deadline, UnitBudget: c.units}
	if m.Ether != nil && m.Ether.Runtime.Limits != nil {
		r := m.Ether.Runtime.Limits
		if r.MemoryBytes > 0 {
			l.MemoryBytes = min(r.MemoryBytes, c.memory)
		}
		if d, err := time.ParseDuration(r.Deadline); err == nil && d > 0 {
			l.Deadline = min(d, c.deadline)
		}
		if r.UnitBudget > 0 {
			l.UnitBudget = min(r.UnitBudget, c.units)
		}
	}
	return l
}
func (m *Manager) grantedRuntimeLimits(man Manifest) *RuntimeLimits {
	if man.RuntimeKind() != "hosted" {
		return nil
	}
	c := m.runtimeCeilings(man.ID)
	l := runtimeLimitsWithCeilings(man, c)
	r := &RuntimeLimits{MemoryBytes: l.MemoryBytes, Deadline: l.Deadline.String(), UnitBudget: l.UnitBudget}
	r.FetchBytes = c.fetch
	r.ArchiveMemberBytes = c.member
	r.ArchiveExpandedBytes = c.expanded
	r.ArchiveMembers = c.members
	if req := man.Ether.Runtime.Limits; req != nil {
		if req.FetchBytes > 0 {
			r.FetchBytes = min(r.FetchBytes, req.FetchBytes)
		}
		if req.ArchiveMemberBytes > 0 {
			r.ArchiveMemberBytes = min(r.ArchiveMemberBytes, req.ArchiveMemberBytes)
		}
		if req.ArchiveExpandedBytes > 0 {
			r.ArchiveExpandedBytes = min(r.ArchiveExpandedBytes, req.ArchiveExpandedBytes)
		}
		if req.ArchiveMembers > 0 {
			r.ArchiveMembers = min(r.ArchiveMembers, req.ArchiveMembers)
		}
	}
	return r
}
func (m *Manager) handleRuntimeLimits(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var req struct {
			Plugin string                     `json:"plugin"`
			Limits config.PluginRuntimeLimits `json:"limits"`
		}
		if err := decode(r, &req); err != nil {
			m.fail(w, err)
			return
		}
		if req.Limits.MemoryBytes > wasm.MaxMemoryBytes || req.Limits.UnitBudget < 0 || req.Limits.FetchBytes < 0 || req.Limits.ArchiveMemberBytes < 0 || req.Limits.ArchiveExpandedBytes < 0 || req.Limits.ArchiveMembers < 0 || req.Limits.ArchiveMembers > 1000000 {
			m.fail(w, fmt.Errorf("memory must be at most 4 GiB and work units must not be negative"))
			return
		}
		if req.Limits.Deadline != "" {
			if d, err := time.ParseDuration(req.Limits.Deadline); err != nil || d <= 0 || d > 365*24*time.Hour {
				m.fail(w, fmt.Errorf("time limit must be positive and at most one year"))
				return
			}
		}
		if req.Plugin != "" {
			if _, err := m.Get(r.Context(), req.Plugin); err != nil {
				m.fail(w, err)
				return
			}
		}
		if m.deps.SaveRuntimeConfig == nil {
			m.fail(w, fmt.Errorf("runtime settings are unavailable"))
			return
		}
		if err := m.deps.SaveRuntimeConfig(req.Plugin, req.Limits); err != nil {
			m.fail(w, err)
			return
		}
		// Closing runtimes cancels jobs so lowered ceilings take effect immediately.
		recs, err := m.List(r.Context())
		if err != nil {
			m.fail(w, err)
			return
		}
		for _, rec := range recs {
			if req.Plugin == "" || req.Plugin == rec.ID {
				m.closeRuntime(rec.ID)
			}
		}
		m.broadcast(r.Context())
	}
	writeJSON(w, http.StatusOK, m.runtimeConfig())
}

// DataLimits exposes owner grants to shared hosted/remote data services.
func (h *host) DataLimits(ctx context.Context) RuntimeLimits {
	rec, err := h.m.Get(ctx, h.plugin)
	if err == nil {
		if l := h.m.grantedRuntimeLimits(rec.Manifest); l != nil {
			return *l
		}
	}
	c := h.m.runtimeCeilings(h.plugin)
	return RuntimeLimits{FetchBytes: c.fetch, ArchiveMemberBytes: c.member, ArchiveExpandedBytes: c.expanded, ArchiveMembers: c.members, Deadline: c.deadline.String()}
}

package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/agent/builtin"
	"github.com/braedonsaunders/ether/internal/agent/tools"
	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/stremio"
)

// registerBuiltinTools registers Ether's built-in agent tools (WS5) into the
// shared tool catalogue and registers their extend.KindTool descriptors in the
// descriptor registry so the console and Studio can list every tool.
func (a *App) registerBuiltinTools(c *tools.Catalogue) error {
	startedAt := time.Now()
	deps := builtin.Deps{
		OnAir:        a.onair,
		Status:       &appStatusService{a: a, startedAt: startedAt},
		Playback:     a.state,
		Resolver:     &appResolverService{a: a},
		Logs:         &appLogService{a: a},
		Catalog:      a.catalog,
		Collections:  a.collections,
		LiveTV:       a.livetv,
		Sources:      &appSourceService{a: a},
		Plugins:      &appPluginService{a: a},
		Approvals:    &appApprovalReader{a: a},
		Profiles:     &appProfileService{a: a},
		QualityRules: &appQualityRulesService{a: a},
		Frontends:    &appFrontendService{a: a},
		Server:       &appServerService{a: a},
		Flows:        &appFlowsService{a: a},
		OwnerUser: func(ctx context.Context) (*core.User, error) {
			if a.users == nil {
				return nil, errors.New("no owner")
			}
			return a.users.Owner(ctx)
		},
		Routes: a.consoleRoutes,
	}
	built := builtin.All(deps)
	if err := c.Register(built...); err != nil {
		return err
	}
	if a.extendRT != nil && a.extendRT.registry != nil {
		if err := registerBuiltinToolDescriptors(a.extendRT.registry, built); err != nil {
			return err
		}
	}
	return nil
}

// builtinToolLabel returns the extend.T label for a built-in tool name using
// literal extend.T("...") calls so web/scripts/i18n.mjs extracts every title.
func builtinToolLabel(name, fallback string) extend.Text {
	switch name {
	case "livetv_channel_apply":
		return extend.T("Apply channel")
	case "look_draft", "livetv_look_draft":
		return extend.T("Draft a look")
	case "channel_summary":
		return extend.T("Channel programming summary")
	case "livetv_look_save":
		return extend.T("Save a channel look")
	case "livetv_channel_look":
		return extend.T("Apply a channel look")
	case "server_status":
		return extend.T("Server status")
	case "health_details":
		return extend.T("Health details")
	case "playback_history":
		return extend.T("Playback history")
	case "resolve_trace":
		return extend.T("Resolve trace")
	case "release_health":
		return extend.T("Release health")
	case "logs_tail":
		return extend.T("Recent logs")
	case "catalog_search":
		return extend.T("Search the catalogue")
	case "title_details":
		return extend.T("Title details")
	case "collection_list":
		return extend.T("Collections")
	case "collection_preview":
		return extend.T("Collection preview")
	case "livetv_guide_match":
		return extend.T("Match with AI")
	case "livetv_channels":
		return extend.T("Live TV channels")
	case "sources_list":
		return extend.T("Sources")
	case "plugins_list":
		return extend.T("Plugins")
	case "plugin_health":
		return extend.T("Plugin health")
	case "approval_status":
		return extend.T("Approval status")
	case "navigate":
		return extend.T("Open a page")
	case "collection_create_from_prompt":
		return extend.T("Create a collection")
	case "release_blocklist":
		return extend.T("Blocklist a release")
	case "release_pin":
		return extend.T("Pin a release")
	case "quality_profiles":
		return extend.T("Quality profiles")
	case "quality_order_set":
		return extend.T("Change quality order")
	case "quality_rules_list":
		return extend.T("Quality rules")
	case "quality_explain":
		return extend.T("Explain quality")
	case "quality_rule_add":
		return extend.T("Add a quality rule")
	case "quality_rule_edit":
		return extend.T("Edit a quality rule")
	case "quality_rule_move":
		return extend.T("Move a quality rule")
	case "quality_defaults_set":
		return extend.T("Change default quality profiles")
	case "format_scores_set":
		return extend.T("Change custom format scores")
	case "rule_add":
		return extend.T("Add a release rule")
	case "source_add_stremio":
		return extend.T("Add a plugin from Stremio")
	case "source_add_m3u":
		return extend.T("Add a playlist source")
	case "frontend_enable":
		return extend.T("Turn a frontend on or off")
	case "plugin_enable":
		return extend.T("Turn a plugin on or off")
	case "server_restart":
		return extend.T("Restart the server")
	case "flows_list":
		return extend.T("Flows")
	case "flow_get":
		return extend.T("Flow details")
	case "flow_create_from_template":
		return extend.T("Create a flow from a template")
	case "flow_enable":
		return extend.T("Turn a flow on or off")
	case "flow_test_run":
		return extend.T("Test-run a flow")
	case "flow_runs":
		return extend.T("Flow runs")
	default:
		return extend.Text{Source: fallback}
	}
}

func registerBuiltinToolDescriptors(r *extend.Registry, list []tools.Tool) error {
	ds := make([]extend.Descriptor, 0, len(list))
	for i, tl := range list {
		group := "Read"
		if !tl.ReadOnly {
			group = "Write"
		}
		d := extend.Descriptor{
			Kind:  extend.KindTool,
			ID:    tl.Name,
			Owner: extend.Builtin,
			Label: builtinToolLabel(tl.Name, tl.Title),
			Group: group,
			Order: i + 1,
		}.WithMeta(map[string]any{
			"name":        tl.Name,
			"readOnly":    tl.ReadOnly,
			"destructive": tl.Destructive,
			"openWorld":   tl.OpenWorld,
		})
		ds = append(ds, d)
	}
	return r.Register(ds...)
}

type appStatusService struct {
	a         *App
	startedAt time.Time
}

func (s *appStatusService) ServerStatus(ctx context.Context) (builtin.ServerStatus, error) {
	cfg := s.a.cfg.Get()
	name := cfg.Server.FriendlyName
	if name == "" {
		name = "Ether"
	}
	enabledIx := 0
	for _, ix := range cfg.Indexers {
		if ix.Enabled {
			enabledIx++
		}
	}
	active := 0
	if s.a.playback != nil {
		active = len((ownerPlayback{Playback: s.a.playback, app: s.a}).activeSessions())
	}
	channels := 0
	if s.a.livetv != nil {
		if list, err := s.a.livetv.Channels(ctx); err == nil {
			channels = len(list)
		}
	}
	return builtin.ServerStatus{
		Version:         s.a.opts.Version,
		ServerName:      name,
		StartedAt:       s.startedAt,
		SetupComplete:   cfg.SetupComplete,
		ActiveSessions:  active,
		IndexersEnabled: enabledIx,
		IndexersTotal:   len(cfg.Indexers),
		LiveChannels:    channels,
	}, nil
}

func (s *appStatusService) Health(ctx context.Context) ([]builtin.HealthCheck, error) {
	cfg := s.a.cfg.Get()
	var out []builtin.HealthCheck

	dbH := s.a.databaseHealth()
	dbStatus := "ok"
	dbDetail := "Database is reachable."
	if dbH.State != "ok" && dbH.State != "" {
		dbStatus = "attention"
		if !dbH.Reachable {
			dbStatus = "error"
		}
		if dbH.Message != "" {
			dbDetail = dbH.Message
		} else if dbH.LastError != "" {
			dbDetail = dbH.LastError
		}
	}
	out = append(out, builtin.HealthCheck{Name: "Database", Status: dbStatus, Detail: dbDetail})

	if s.a.cache != nil {
		used, budget := s.a.cache.Usage()
		detail := fmt.Sprintf("Mode %s, %d MiB used", firstNonEmptyStr(cfg.Cache.Mode, "demand"), used/(1<<20))
		if budget > 0 {
			detail = fmt.Sprintf("%s of %d MiB budget", detail, budget/(1<<20))
		}
		out = append(out, builtin.HealthCheck{Name: "Cache", Status: "ok", Detail: detail})
	}

	enabledSources := 0
	for _, ix := range cfg.Indexers {
		if ix.Enabled {
			enabledSources++
		}
	}
	for _, p := range cfg.Usenet.Providers {
		if p.Enabled {
			enabledSources++
		}
	}
	for _, p := range cfg.Debrid.Providers {
		if p.Enabled {
			enabledSources++
		}
	}
	srcStatus := "ok"
	srcDetail := fmt.Sprintf("%d enabled release sources.", enabledSources)
	if enabledSources == 0 {
		srcStatus = "attention"
		srcDetail = "No release sources are enabled."
	}
	out = append(out, builtin.HealthCheck{Name: "Sources", Status: srcStatus, Detail: srcDetail})

	if s.a.transcoder != nil {
		hw := s.a.transcoder.HWAccel()
		cap := s.a.transcoder.Capability()
		st := "ok"
		det := fmt.Sprintf("FFmpeg %s, hardware acceleration: %s", s.a.transcoder.Version(), firstNonEmptyStr(hw.Name, "none"))
		if cap.PlaybackError != "" {
			st = "error"
			det = cap.PlaybackError
		} else if cap.Notice != "" {
			st = "attention"
			det = cap.Notice
		}
		out = append(out, builtin.HealthCheck{Name: "Transcoder", Status: st, Detail: det})
	}

	return out, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

type appResolverService struct {
	a *App
}

func (r *appResolverService) History(ctx context.Context, f resolver.HistoryFilter) ([]resolver.HistoryRecord, int, error) {
	return r.a.resolver.History(ctx, f)
}

func (r *appResolverService) HistoryEntry(ctx context.Context, id int64) (resolver.HistoryRecord, error) {
	return r.a.resolver.HistoryEntry(ctx, id)
}

func (r *appResolverService) LookupRelease(id string) (core.Release, bool) {
	if rel, ok := r.a.resolver.LookupRelease(id); ok {
		return rel, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, _, err := r.a.resolver.History(ctx, resolver.HistoryFilter{Limit: 50})
	if err != nil {
		return core.Release{}, false
	}
	for _, row := range rows {
		if row.ReleaseID == id {
			return core.Release{ID: row.ReleaseID, Name: row.ReleaseName, Indexer: row.Indexer, Size: row.Size}, true
		}
		entry, eerr := r.a.resolver.HistoryEntry(ctx, row.ID)
		if eerr != nil || entry.Trace == nil {
			continue
		}
		for _, c := range entry.Trace.Candidates {
			if c.ReleaseID == id {
				return core.Release{ID: c.ReleaseID, Name: c.Name, Indexer: c.Indexer, Size: c.Size}, true
			}
		}
	}
	return core.Release{}, false
}

func (r *appResolverService) ReleaseHealth(ctx context.Context, releaseID string, titleKey core.RatingKey) (builtin.ReleaseHealth, error) {
	_, known := r.a.resolver.LookupRelease(releaseID)
	blocked, err := r.a.resolver.ReleaseBlocked(ctx, releaseID)
	if err != nil {
		return builtin.ReleaseHealth{}, err
	}
	out := builtin.ReleaseHealth{Known: known, Blocked: blocked}
	if blocked {
		out.Status = "blocked"
		if entries, _, berr := r.a.resolver.Blocklist(ctx, resolver.BlockFilter{Query: releaseID, Limit: 20}); berr == nil {
			for _, e := range entries {
				if e.ReleaseID == releaseID {
					out.Known = true
					out.Reason = e.Reason
					out.CheckedAt = e.UpdatedAt
					break
				}
			}
		}
	}
	for _, p := range r.a.resolver.Pins() {
		if p.Release.ID == releaseID && (titleKey == 0 || p.TitleKey == titleKey) {
			out.Pinned = true
			out.Known = true
			if out.Status == "" && p.LastStatus != "" {
				out.Status = string(p.LastStatus)
				out.Reason = p.LastReason
				out.CheckedAt = p.LastCheckedAt
			}
			break
		}
	}
	if out.Status == "" {
		rows, _, herr := r.a.resolver.History(ctx, resolver.HistoryFilter{TitleKey: titleKey, Limit: 5})
		if herr == nil {
			for _, row := range rows {
				entry, eerr := r.a.resolver.HistoryEntry(ctx, row.ID)
				if eerr != nil || entry.Trace == nil {
					continue
				}
				for _, c := range entry.Trace.Candidates {
					if c.ReleaseID == releaseID {
						out.Known = true
						if c.HealthChecked {
							out.CheckedAt = entry.At
							if c.Healthy {
								out.Status = "healthy"
							} else {
								out.Status = "unhealthy"
							}
							out.Reason = c.Reason
						} else if c.Rejected && c.Reason != "" {
							out.Status = "rejected"
							out.Reason = c.Reason
						}
						break
					}
				}
				if out.Status != "" {
					break
				}
			}
		}
	}
	return out, nil
}

func (r *appResolverService) Block(ctx context.Context, req resolver.BlockRequest, reason string) (resolver.BlockEntry, error) {
	entry, err := r.a.resolver.Block(ctx, req, reason)
	if err == nil && entry.TitleKey != 0 && r.a.playback != nil {
		r.a.playback.InvalidateTitleEverywhere(entry.TitleKey)
	}
	return entry, err
}

func (r *appResolverService) PinRelease(ctx context.Context, titleKey core.RatingKey, releaseID, note string) error {
	rel, ok := r.LookupRelease(releaseID)
	if !ok {
		return resolver.ErrUnknownRelease
	}
	var titleName string
	if r.a.catalog != nil {
		if t, err := r.a.catalog.Get(ctx, titleKey); err == nil && t != nil {
			titleName = t.Title
		}
	}
	if _, err := r.a.resolver.Pin(ctx, titleKey, titleName, rel, note); err != nil {
		return err
	}
	if r.a.playback != nil {
		r.a.playback.InvalidateTitleEverywhere(titleKey)
	}
	return nil
}

type appLogService struct {
	a *App
}

func (l *appLogService) Tail(limit int, level, query string) []builtin.LogLine {
	if l.a.logs == nil {
		return nil
	}
	entries := l.a.logs.Entries(limit, level, query)
	out := make([]builtin.LogLine, 0, len(entries))
	for _, e := range entries {
		out = append(out, builtin.LogLine{
			Time:    e.Time,
			Level:   e.Level,
			Message: e.Message,
			Attrs:   e.Attrs,
		})
	}
	return out
}

type appSourceService struct {
	a *App
}

func (s *appSourceService) Sources(ctx context.Context) ([]builtin.SourceInfo, error) {
	cfg := s.a.cfg.Get()
	var out []builtin.SourceInfo
	for _, ix := range cfg.Indexers {
		kind := ix.Kind
		if kind == "" {
			kind = config.IndexerKindNewznab
		}
		class := "indexer"
		if ix.IsStremio() {
			class = "addon"
		}
		out = append(out, builtin.SourceInfo{
			Name:    ix.Name,
			Kind:    kind,
			Class:   class,
			Enabled: ix.Enabled,
			Host:    bareHost(ix.URL),
			Health:  enabledHealth(ix.Enabled),
		})
	}
	for _, p := range cfg.Usenet.Providers {
		out = append(out, builtin.SourceInfo{
			Name:    firstNonEmptyStr(p.Name, p.Host),
			Kind:    "usenet",
			Class:   "usenet",
			Enabled: p.Enabled,
			Host:    p.Host,
			Health:  enabledHealth(p.Enabled),
		})
	}
	for _, p := range cfg.Debrid.Providers {
		out = append(out, builtin.SourceInfo{
			Name:    firstNonEmptyStr(p.Name, p.Kind),
			Kind:    firstNonEmptyStr(p.Kind, "debrid"),
			Class:   "debrid",
			Enabled: p.Enabled,
			Health:  enabledHealth(p.Enabled),
		})
	}
	return out, nil
}

func (s *appSourceService) AddStremio(ctx context.Context, addonURL string) (builtin.SourceInfo, error) {
	base, err := stremio.BaseURL(addonURL)
	if err != nil {
		return builtin.SourceInfo{}, tools.Userf("%s", err.Error())
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	id := "stremio-" + hex.EncodeToString(b[:])
	name := base.Host
	ix := config.Indexer{
		ID:       id,
		Name:     name,
		URL:      base.String(),
		Kind:     config.IndexerKindStremio,
		Protocol: "torrent",
		Enabled:  true,
	}
	if err := s.a.cfg.Update(func(c *config.Config) error {
		c.Indexers = append(c.Indexers, ix)
		return nil
	}); err != nil {
		return builtin.SourceInfo{}, err
	}
	return builtin.SourceInfo{
		Name:    name,
		Kind:    config.IndexerKindStremio,
		Class:   "addon",
		Enabled: true,
		Host:    base.Host,
		Health:  "unknown",
	}, nil
}

func bareHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

func enabledHealth(enabled bool) string {
	if enabled {
		return "unknown"
	}
	return "disabled"
}

type appPluginService struct {
	a *App
}

func (p *appPluginService) mgr() *plugins.Manager {
	if p.a.pluginsRT == nil {
		return nil
	}
	return p.a.pluginsRT.mgr
}

func (p *appPluginService) Plugins(ctx context.Context) ([]builtin.PluginInfo, error) {
	mgr := p.mgr()
	if mgr == nil {
		return nil, nil
	}
	recs, err := mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]builtin.PluginInfo, 0, len(recs))
	for _, r := range recs {
		h, _ := mgr.HealthOf(ctx, r.ID)
		out = append(out, toPluginInfo(r, h))
	}
	return out, nil
}

func (p *appPluginService) PluginHealth(ctx context.Context, id string) (builtin.PluginInfo, error) {
	mgr := p.mgr()
	if mgr == nil {
		return builtin.PluginInfo{}, core.ErrNotFound
	}
	r, err := mgr.Get(ctx, id)
	if err != nil {
		if errors.Is(err, plugins.ErrNotFound) {
			return builtin.PluginInfo{}, core.ErrNotFound
		}
		return builtin.PluginInfo{}, err
	}
	h, err := mgr.HealthOf(ctx, id)
	if err != nil && !errors.Is(err, plugins.ErrNotFound) {
		return builtin.PluginInfo{}, err
	}
	return toPluginInfo(r, h), nil
}

func (p *appPluginService) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	mgr := p.mgr()
	if mgr == nil {
		return core.ErrNotFound
	}
	err := mgr.SetEnabled(ctx, id, enabled)
	if errors.Is(err, plugins.ErrNotFound) {
		return tools.Userf("no plugin with that id")
	}
	return err
}

func toPluginInfo(r plugins.Record, h plugins.Health) builtin.PluginInfo {
	status := h.Status
	if !r.Enabled {
		status = "disabled"
	} else if status == "" {
		status = "ok"
	}
	var detail string
	switch status {
	case "error":
		detail = firstNonEmptyStr(h.Reason, h.LastError)
	case "disabled":
		if h.Status == "disabled" {
			detail = h.Reason
		}
	case "ok":
	default:
		detail = h.Reason
	}
	return builtin.PluginInfo{
		ID:      r.ID,
		Name:    r.Name,
		Version: r.Version,
		Kind:    r.Kind,
		Enabled: r.Enabled,
		Health:  status,
		Detail:  detail,
	}
}

type appApprovalReader struct {
	a *App
}

func (ar *appApprovalReader) Approval(ctx context.Context, id string) (builtin.ApprovalInfo, error) {
	if ar.a.db == nil {
		return builtin.ApprovalInfo{}, builtin.ErrApprovalNotFound
	}
	var info builtin.ApprovalInfo
	var decisionNote, failureReason, threadID string
	var createdMs, decidedMs int64
	err := ar.a.db.QueryRowContext(ctx,
		`SELECT id, tool_name, category, description, status, decision_note, failure_reason, created_at, decided_at, thread_id FROM agent_approvals WHERE id=?`, id).
		Scan(&info.ID, &info.Tool, &info.Category, &info.Description, &info.Status, &decisionNote, &failureReason, &createdMs, &decidedMs, &threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return builtin.ApprovalInfo{}, builtin.ErrApprovalNotFound
	}
	if err != nil {
		return builtin.ApprovalInfo{}, err
	}
	if info.Tool == "source_add_stremio" && info.Status == "approved" {
		var secretStatus string
		err := ar.a.db.QueryRowContext(ctx, `SELECT status FROM agent_secret_requests WHERE thread_id=? ORDER BY created_at DESC LIMIT 1`, threadID).Scan(&secretStatus)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return builtin.ApprovalInfo{}, err
		}
		if secretStatus != "stored" {
			info.Status = "pending_secret"
			decisionNote = "The owner must submit the add-on URL in the console before the source is added."
		}
	}
	info.Note = firstNonEmptyStr(decisionNote, failureReason)
	if createdMs > 0 {
		info.CreatedAt = time.UnixMilli(createdMs).UTC()
	}
	if decidedMs > 0 {
		t := time.UnixMilli(decidedMs).UTC()
		info.DecidedAt = &t
	}
	return info, nil
}

type appProfileService struct {
	a *App
}

func (ps *appProfileService) Profiles(ctx context.Context) ([]builtin.ProfileInfo, error) {
	cfg := ps.a.cfg.Get()
	out := make([]builtin.ProfileInfo, 0, len(cfg.Quality.Profiles))
	for _, p := range cfg.Quality.Profiles {
		out = append(out, builtin.ProfileInfo{
			Name:               p.Name,
			Scores:             maps.Clone(p.CustomFormatScores),
			Rules:              append([]core.ReleaseRule(nil), p.ReleaseRules...),
			QualityOrder:       config.CloneQualityOrder(p.QualityOrder),
			Language:           p.Language,
			AudioLanguageMode:  p.AudioLanguageMode,
			PreferredLanguages: append([]string(nil), p.PreferredLanguages...),
			MaxHeight:          p.MaxHeight,
			MinHeight:          p.MinHeight,
			AllowedQualities:   append([]string(nil), p.AllowedQualities...),
		})
	}
	return out, nil
}

func (ps *appProfileService) SetQualityOrder(ctx context.Context, profile string, order []core.QualityItem) error {
	return ps.a.saveValidated(func(c *config.Config) error {
		for i := range c.Quality.Profiles {
			p := &c.Quality.Profiles[i]
			if strings.EqualFold(p.Name, profile) {
				p.QualityOrder = config.CloneQualityOrder(order)
				return nil
			}
		}
		return tools.Userf("no profile named %q", profile)
	})
}

func (ps *appProfileService) SetFormatScores(ctx context.Context, profile string, scores map[string]int) error {
	return ps.a.saveValidated(func(c *config.Config) error {
		for i := range c.Quality.Profiles {
			p := &c.Quality.Profiles[i]
			if strings.EqualFold(p.Name, profile) {
				if p.CustomFormatScores == nil {
					p.CustomFormatScores = map[string]int{}
				}
				for id, v := range scores {
					if v == 0 {
						delete(p.CustomFormatScores, id)
					} else {
						p.CustomFormatScores[id] = v
					}
				}
				return nil
			}
		}
		return tools.Userf("no profile named %q", profile)
	})
}

func (ps *appProfileService) AddRule(ctx context.Context, profile string, rule core.ReleaseRule) error {
	return ps.a.saveValidated(func(c *config.Config) error {
		for i := range c.Quality.Profiles {
			if strings.EqualFold(c.Quality.Profiles[i].Name, profile) {
				p := &c.Quality.Profiles[i]
				baseID := rule.ID
				if baseID == "" {
					baseID = "rule"
				}
				id := baseID
				for n := 2; hasRuleID(p.ReleaseRules, id); n++ {
					id = fmt.Sprintf("%s-%d", baseID, n)
				}
				rule.ID = id
				p.ReleaseRules = append(p.ReleaseRules, rule)
				return nil
			}
		}
		return tools.Userf("no profile named %q", profile)
	})
}

func hasRuleID(rules []core.ReleaseRule, id string) bool {
	for _, r := range rules {
		if r.ID == id {
			return true
		}
	}
	return false
}

type appQualityRulesService struct {
	a *App
}

func (qs *appQualityRulesService) QualityRules(ctx context.Context, includeDevices bool) (builtin.QualityRulesSnapshot, error) {
	cfg := qs.a.cfg.Get()
	profiles := make([]string, 0, len(cfg.Quality.Profiles))
	for _, p := range cfg.Quality.Profiles {
		profiles = append(profiles, p.Name)
	}
	var libraries []builtin.QualityLibraryRef
	if qs.a.libraries != nil {
		libs, err := qs.a.libraries.List(ctx)
		if err != nil {
			return builtin.QualityRulesSnapshot{}, err
		}
		libraries = make([]builtin.QualityLibraryRef, 0, len(libs))
		for _, lib := range libs {
			libraries = append(libraries, builtin.QualityLibraryRef{
				ID:             int64(lib.ID),
				Name:           lib.Name,
				MediaType:      string(lib.MediaType),
				ChosenByPlugin: lib.Source == core.LibraryPlugin,
			})
		}
	}
	var viewers []builtin.QualityViewerRef
	if qs.a.users != nil {
		users, err := qs.a.users.AllUsers(ctx)
		if err != nil {
			return builtin.QualityRulesSnapshot{}, err
		}
		viewers = make([]builtin.QualityViewerRef, 0, len(users))
		for _, u := range users {
			viewers = append(viewers, builtin.QualityViewerRef{
				ID:   u.ID,
				Name: firstNonEmptyStr(u.Title, u.Username, fmt.Sprintf("Viewer %d", u.ID)),
			})
		}
	}
	snap := builtin.QualityRulesSnapshot{
		Default:           cfg.Quality.Default,
		DefaultAway:       cfg.Quality.DefaultAway,
		DefaultLimits:     cfg.Quality.DefaultLimits,
		DefaultAwayLimits: cfg.Quality.DefaultAwayLimits,
		Profiles:          profiles,
		Rules:             cfg.Quality.Rules,
		Libraries:         libraries,
		Viewers:           viewers,
	}
	if includeDevices {
		regApps := clientreg.Apps()
		apps := make([]builtin.QualityDeviceAppRef, 0, len(regApps))
		for _, item := range regApps {
			apps = append(apps, builtin.QualityDeviceAppRef{
				ID:       clientreg.AppID(item),
				Name:     item.Name,
				Protocol: item.Protocol,
			})
		}
		clients := []builtin.QualityDeviceClientRef{}
		if qs.a.db != nil {
			if seen, err := qs.a.seenPlaybackDevices(ctx); err == nil {
				clients = make([]builtin.QualityDeviceClientRef, 0, len(seen))
				for _, d := range seen {
					id := clientreg.Identity{
						Product:    d.App,
						Platform:   d.Platform,
						Device:     d.Device,
						DeviceName: d.Name,
					}
					name := firstNonEmptyStr(d.Name, d.App, d.Device, d.ClientIdentifier)
					var lastSeen string
					if !d.LastSeen.IsZero() {
						lastSeen = d.LastSeen.UTC().Format(time.RFC3339)
					}
					clients = append(clients, builtin.QualityDeviceClientRef{
						ID:       d.ClientIdentifier,
						Name:     name,
						Group:    clientreg.Class(id),
						Type:     clientreg.DeviceType(id),
						App:      clientreg.AppFor("", id),
						Viewer:   d.Viewer,
						LastSeen: lastSeen,
					})
				}
			}
		}
		snap.Devices = &builtin.QualityDevicesRef{
			Groups:  clientreg.Classes(),
			Types:   clientreg.DeviceTypes(),
			Apps:    apps,
			Clients: clients,
		}
	}
	return snap, nil
}

func (qs *appQualityRulesService) ExplainQuality(ctx context.Context, req builtin.QualityExplainRequest) (resolver.QualityAnswer, error) {
	if req.Kind != "" && req.Kind != config.KindMovie && req.Kind != config.KindEpisode {
		return resolver.QualityAnswer{}, tools.Userf("kind must be movie or episode")
	}
	var client qualityClient
	var exactClientID string
	lookupSeenClient := func(targetID string) bool {
		if qs.a.db == nil {
			return false
		}
		seen, err := qs.a.seenPlaybackDevices(ctx)
		if err != nil {
			return false
		}
		for _, d := range seen {
			if d.ClientIdentifier == targetID {
				exactClientID = d.ClientIdentifier
				client = qualityClient{
					ID:       d.ClientIdentifier,
					Name:     d.Name,
					Product:  d.App,
					Platform: d.Platform,
					Device:   d.Device,
				}
				return true
			}
		}
		return false
	}
	switch {
	case req.ClientID == "any" || (req.ClientID == "" && req.Client == nil):
		// Generic client with no assumed app.
	case req.ClientID != "":
		found := lookupSeenClient(req.ClientID)
		if !found {
			for _, c := range qualityClients() {
				if c.ID == req.ClientID {
					client, found = c, true
					break
				}
			}
		}
		if !found {
			if dev, ok := resolver.ParseSummaryDevice(req.ClientID); ok && dev.Field != "" {
				switch dev.Field {
				case "client":
					found = lookupSeenClient(dev.Value)
				case "type":
					for _, d := range clientreg.Devices() {
						if d.TypeID() == dev.Value {
							client = qualityClient{ID: req.ClientID, Name: d.Name, Product: d.Identity.Product, Platform: d.Identity.Platform, Device: d.Identity.Device, Model: d.Identity.Model}
							found = true
							break
						}
					}
				case "app":
					for _, appItem := range clientreg.Apps() {
						if clientreg.AppID(appItem) == dev.Value {
							prod := firstNonEmptyStr(appItem.Identity.Product, appItem.Product, appItem.Name)
							client = qualityClient{ID: req.ClientID, Name: appItem.Name, Product: prod, Platform: appItem.Identity.Platform, Device: appItem.Identity.Device, Protocol: core.Protocol(appItem.Protocol)}
							found = true
							break
						}
					}
				}
			}
		}
		if !found {
			return resolver.QualityAnswer{}, tools.Userf("no such device or app %q; call quality_rules_list with includeDevices=true to see valid ids", req.ClientID)
		}
	case req.Client != nil && strings.TrimSpace(req.Client.Product) != "":
		client = qualityClient{
			Product:  strings.TrimSpace(req.Client.Product),
			Platform: strings.TrimSpace(req.Client.Platform),
			Device:   strings.TrimSpace(req.Client.Device),
			Protocol: req.Client.Protocol,
		}
	default:
		return resolver.QualityAnswer{}, tools.Userf("choose a device or app")
	}
	if len(client.Product) > 128 || len(client.Platform) > 128 || len(client.Device) > 128 {
		return resolver.QualityAnswer{}, tools.Userf("client fields are too long")
	}

	var u *core.User
	if req.ViewerID > 0 {
		if qs.a.users == nil {
			return resolver.QualityAnswer{}, tools.Userf("no such viewer %d", req.ViewerID)
		}
		loaded, err := qs.a.users.ActiveUser(ctx, req.ViewerID)
		if err != nil || loaded == nil {
			return resolver.QualityAnswer{}, tools.Userf("no such viewer %d", req.ViewerID)
		}
		u = loaded
	}

	var title *core.Title
	var inLib resolver.Membership
	var chosenByPlugin string
	kind := req.Kind
	if req.TitleKey != 0 {
		if qs.a.catalog == nil {
			return resolver.QualityAnswer{}, tools.Userf("no such movie or episode")
		}
		loaded, err := qs.a.catalog.CachedGet(ctx, req.TitleKey)
		if err != nil || loaded == nil || (loaded.Type != core.TypeMovie && loaded.Type != core.TypeEpisode) {
			return resolver.QualityAnswer{}, tools.Userf("no such movie or episode")
		}
		title = loaded
		if qs.a.libProfiles != nil {
			inLib = func(id core.LibraryID) bool {
				return qs.a.libProfiles.InLibrary(ctx, title, id)
			}
		}
	} else if req.LibraryID > 0 {
		if qs.a.libraries == nil {
			return resolver.QualityAnswer{}, tools.Userf("no such library %d", req.LibraryID)
		}
		lib, err := qs.a.libraries.Get(ctx, core.LibraryID(req.LibraryID))
		if err != nil {
			return resolver.QualityAnswer{}, tools.Userf("no such library %d", req.LibraryID)
		}
		inLib = func(id core.LibraryID) bool { return id == lib.ID }
		if lib.Source == core.LibraryPlugin {
			chosenByPlugin = qs.a.pluginDisplayName(ctx, lib.PluginID)
		}
		if kind == "" {
			switch lib.MediaType {
			case core.TypeMovie:
				kind = config.KindMovie
			case core.TypeShow:
				kind = config.KindEpisode
			}
		}
	}

	cfg := qs.a.cfg.Read()
	devName := firstNonEmptyStr(client.Name, client.Device)
	pr := &plex.Request{
		ClientID:   exactClientID,
		Product:    client.Product,
		Platform:   client.Platform,
		Device:     client.Device,
		DeviceName: devName,
		Model:      client.Model,
		Remote:     req.Remote,
		User:       u,
	}
	declared := pr.ClientCapsWith(0, cfg.Clients.Overrides)
	builtinCaps := pr.ClientCapsWith(0, nil)
	if frontendHostPolicy(client.Protocol).ProfileQualityEvaluation {
		info := caps.DeviceInfo{
			Protocol: client.Protocol,
			ClientID: exactClientID,
			Product:  client.Product,
			Platform: client.Platform,
			Device:   client.Device,
			Remote:   req.Remote,
		}
		neutral, err := (caps.Builder{}).Build(ctx, info, caps.DeviceProfile{}, nil, "")
		if err != nil {
			return resolver.QualityAnswer{}, tools.Userf("cannot evaluate app")
		}
		builtinCaps = playbackFloor(neutral, nil)
		declared = playbackFloor(neutral, cfg.Clients.Overrides)
	} else if client.Product == "" {
		declared = core.ClientCaps{ClientID: exactClientID, Remote: req.Remote, HDR10: true, DolbyVision: true}
		builtinCaps = declared
	}

	q := resolver.QualityQuestion{
		Quality:        cfg.Quality,
		Caps:           declared,
		Builtin:        &builtinCaps,
		User:           u,
		Title:          title,
		Kind:           kind,
		InLibrary:      inLib,
		ChosenByPlugin: chosenByPlugin,
	}
	if qs.a.safety != nil && u != nil {
		policy := qs.a.safety.ForUser(u)
		q.RemoteDisabled = policy.RemoteDisabled
	}
	return resolver.ExplainQuality(q), nil
}

func (qs *appQualityRulesService) ValidateQuality(ctx context.Context, mutate func(*config.Quality) error) (config.Quality, error) {
	next := qs.a.cfg.Get()
	if err := mutate(&next.Quality); err != nil {
		return config.Quality{}, err
	}
	config.AssignRuleIDs(&next.Quality)
	if errs := append(admin.ValidateConfig(next), qs.a.validateExtra(next)...); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Field+" "+e.Message)
		}
		return config.Quality{}, tools.Userf("%s", strings.Join(msgs, "; "))
	}
	return next.Quality, nil
}

func (qs *appQualityRulesService) UpdateQuality(ctx context.Context, mutate func(*config.Quality) error) (config.Quality, error) {
	err := qs.a.saveValidated(func(c *config.Config) error {
		return mutate(&c.Quality)
	})
	if err != nil {
		if errors.Is(err, resolver.ErrImportRejected) {
			msg := strings.TrimPrefix(err.Error(), resolver.ErrImportRejected.Error()+": ")
			return config.Quality{}, tools.Userf("%s", msg)
		}
		return config.Quality{}, err
	}
	return qs.a.cfg.Get().Quality, nil
}

type appFrontendService struct {
	a *App
}

func (fs *appFrontendService) Frontends(ctx context.Context) ([]builtin.FrontendInfo, error) {
	if fs.a.frontends != nil && fs.a.frontends.control != nil {
		list, err := fs.a.frontends.control.List(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]builtin.FrontendInfo, 0, len(list))
		for _, f := range list {
			out = append(out, builtin.FrontendInfo{
				ID:      string(f.Descriptor.ID),
				Name:    f.Descriptor.Name,
				Enabled: f.Enabled,
			})
		}
		return out, nil
	}
	cfg := fs.a.cfg.Get()
	var out []builtin.FrontendInfo
	// Wire the built-in frontend identities for the agent app list.
	for _, id := range []string{"plex", "jellyfin", "emby", "stremio", "dlna", "webdav", "m3u"} {
		enabled := cfg.Frontends[id].Enabled
		if cfg.Frontends == nil && frontendHostPolicy(frontend.Protocol(id)).MainListener {
			enabled = true
		}
		out = append(out, builtin.FrontendInfo{ID: id, Name: id, Enabled: enabled})
	}
	return out, nil
}

func (fs *appFrontendService) SetFrontendEnabled(ctx context.Context, id string, enabled bool) error {
	if fs.a.frontends != nil && fs.a.frontends.control != nil {
		return fs.a.frontends.control.Toggle(ctx, frontend.Protocol(id), enabled)
	}
	return fs.a.cfg.Update(func(c *config.Config) error {
		if c.Frontends == nil {
			c.Frontends = map[string]config.Frontend{}
			if !frontendHostPolicy(frontend.Protocol(id)).MainListener {
				c.Frontends["plex"] = config.Frontend{Enabled: true} // Preserve the default main-listener registration.
			}
		}
		v := c.Frontends[id]
		v.Enabled = enabled
		c.Frontends[id] = v
		return nil
	})
}

type appServerService struct {
	a *App
}

func (ss *appServerService) Restart(ctx context.Context, whenIdle bool) error {
	return ss.a.scheduleRestart("restart approved in Ask Ether", whenIdle)
}

func (a *App) consoleRoutes() []builtin.RouteInfo {
	out := []builtin.RouteInfo{
		{Route: "/", Label: "Overview"},
		{Route: "/sources", Label: "Sources"},
		{Route: "/releases", Label: "Releases"},
		{Route: "/collections", Label: "Collections"},
		{Route: "/livetv", Label: "Live TV"},
		{Route: "/viewers", Label: "Viewers"},
		{Route: "/apps", Label: "Apps"},
		{Route: "/integrations", Label: "Integrations"},
		{Route: "/plugins", Label: "Plugins"},
		{Route: "/flows", Label: "Flows"},
		{Route: "/notifications", Label: "Notifications"},
		{Route: "/logs", Label: "Logs"},
		{Route: "/settings", Label: "Settings"},
	}
	if a.extendRT != nil && a.extendRT.registry != nil {
		seen := map[string]bool{}
		for _, r := range out {
			seen[r.Route] = true
		}
		for _, d := range a.extendRT.registry.List(extend.KindPage) {
			var meta struct {
				Route string `json:"route"`
			}
			_, _ = extend.MetaOf[struct {
				Route string `json:"route"`
			}](d)
			if m, err := extend.MetaOf[map[string]any](d); err == nil {
				if rt, ok := m["route"].(string); ok {
					meta.Route = rt
				}
			}
			if meta.Route == "" || seen[meta.Route] {
				continue
			}
			label := d.Label.Source
			if label == "" {
				label = d.Label.Resolved
			}
			if label == "" {
				label = d.ID
			}
			seen[meta.Route] = true
			out = append(out, builtin.RouteInfo{Route: meta.Route, Label: label})
		}
	}
	return out
}

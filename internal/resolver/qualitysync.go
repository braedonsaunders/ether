package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// QualitySyncReport is the outcome of one imported-profile sync.
type QualitySyncReport struct {
	ProfilesUpdated int      `json:"profilesUpdated"`
	FormatsAdded    int      `json:"formatsAdded"`
	FormatsUpdated  int      `json:"formatsUpdated"`
	FormatsRemoved  int      `json:"formatsRemoved"`
	Warnings        []string `json:"warnings"`
}

// QualitySyncDeps wires an imported-profile sync.
type QualitySyncDeps struct {
	// Guides and Dictionarry are the long-lived guide clients; nil makes a
	// profile from that guide unsyncable.
	Guides      *TrashGuides
	Dictionarry *Dictionarry
	// Config returns the current settings (a snapshot is fine: Save
	// re-applies the change to the latest settings).
	Config func() config.Config
	// Save applies a mutation through the owner's validated config path.
	Save   func(mutate func(*config.Config) error) error
	Logger *slog.Logger
	Now    func() time.Time
}

// QualitySyncer keeps imported profiles and their format definitions in
// step with their source guide. One sync runs at a time; the config store's
// update/retry semantics protect a concurrent manual import.
type QualitySyncer struct {
	deps QualitySyncDeps
	sem  chan struct{}
}

// NewQualitySyncer returns a syncer for the given sources.
func NewQualitySyncer(deps QualitySyncDeps) *QualitySyncer {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &QualitySyncer{deps: deps, sem: make(chan struct{}, 1)}
}

// SyncDue reports whether the configured sync should run at now. An
// enabled sync with no start time is due at once; a missing interval means
// once a day.
func SyncDue(q config.QualitySync, now time.Time) bool {
	if !q.Enabled {
		return false
	}
	interval := q.Interval.D()
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return q.LastRun.IsZero() || !now.Before(q.LastRun.Add(interval))
}

// Sync fetches every source-backed imported profile and updates it in
// place. A profile whose fetch fails or whose import cannot work is skipped
// with a warning; the others still update. All changes (and the run's
// LastRun/LastError) go through one validated save.
func (s *QualitySyncer) Sync(ctx context.Context) (QualitySyncReport, error) {
	report := QualitySyncReport{Warnings: []string{}}
	if s == nil {
		return report, errors.New("quality profile sync is not configured")
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return report, ctx.Err()
	}
	if s.deps.Config == nil || s.deps.Save == nil {
		return report, errors.New("quality profile sync is not configured")
	}
	cfg := s.deps.Config()
	type target struct {
		name  string
		media string
		imp   core.ProfileImport
		plan  QualityImportPlan
	}
	var targets []target
	failed := false
	for _, prof := range cfg.Quality.Profiles {
		imp := prof.Import
		if imp == nil || strings.TrimSpace(imp.Source) == "" {
			continue
		}
		plan, err := s.plan(ctx, *imp, cfg)
		if err != nil {
			failed = true
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %s", prof.Name, syncReason(err)))
			continue
		}
		missing := false
		for _, format := range plan.Formats {
			missing = missing || format.Status == FormatMissing
		}
		if missing {
			failed = true
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: incomplete custom format definitions; profile was kept", prof.Name))
			continue
		}
		if plan.Unusable != "" {
			failed = true
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %s", prof.Name, plan.Unusable))
			continue
		}
		targets = append(targets, target{name: prof.Name, media: prof.MediaType, imp: *imp, plan: plan})
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	now := s.deps.Now().UTC()
	err := s.deps.Save(func(c *config.Config) error {
		// Update may re-run this mutation against fresher settings.
		report.ProfilesUpdated, report.FormatsAdded, report.FormatsUpdated, report.FormatsRemoved = 0, 0, 0, 0
		desired := map[string]bool{}
		for _, t := range targets {
			for _, def := range t.plan.Definitions {
				desired[formatID(def)] = true
			}
			s.apply(c, t.name, t.media, t.imp, t.plan, now, &report)
		}
		if failed {
			// A failed or unusable profile still references its old formats;
			// pruning against this partial desired set could delete them.
			report.FormatsRemoved = 0
		} else {
			report.FormatsRemoved = pruneManagedFormats(c, desired)
		}
		if c.Quality.Sync.Enabled && c.Quality.Sync.Interval.D() <= 0 {
			// A config that predates the interval keeps working on the
			// documented once-a-day default.
			c.Quality.Sync.Interval = config.Duration(24 * time.Hour)
		}
		c.Quality.Sync.LastRun = now
		if len(report.Warnings) > 0 {
			c.Quality.Sync.LastError = strings.Join(report.Warnings, " ")
		} else {
			c.Quality.Sync.LastError = ""
		}
		return nil
	})
	if err != nil {
		// The run still happened; keep the reason visible in settings.
		_ = s.deps.Save(func(c *config.Config) error {
			c.Quality.Sync.LastError = err.Error()
			return nil
		})
		return report, err
	}
	return report, nil
}

// plan fetches imp's current source profile and formats.
func (s *QualitySyncer) plan(ctx context.Context, imp core.ProfileImport, cfg config.Config) (QualityImportPlan, error) {
	app := strings.ToLower(strings.TrimSpace(imp.App))
	if !validApp(app) {
		return QualityImportPlan{}, fmt.Errorf("the saved profile's app %q is not radarr or sonarr", imp.App)
	}
	source := strings.ToLower(strings.TrimSpace(imp.Source))
	profileID := strings.TrimSpace(imp.Profile)
	if profileID == "" {
		return QualityImportPlan{}, errors.New("the saved profile has no source profile id")
	}
	switch source {
	case ImportSourceTrash:
		if s.deps.Guides == nil {
			return QualityImportPlan{}, errors.New("TRaSH Guides is not configured")
		}
		data, err := s.deps.Guides.Profile(ctx, app, profileID)
		if err != nil {
			return QualityImportPlan{}, err
		}
		qimp, err := ImportQualityProfile(data)
		if err != nil {
			return QualityImportPlan{}, err
		}
		qimp.Source, qimp.ProfileID = ImportSourceTrash, profileID
		if qimp.TrashID == "" {
			qimp.TrashID = imp.TrashID
		}
		// Any group read failure would make the scores a partial snapshot;
		// skip this profile rather than overwrite it with less.
		extra, err := s.deps.Guides.ProfileGroupFormats(ctx, app, qimp.TrashID)
		if errors.Is(err, ErrTrashPartial) || (err != nil && !errors.Is(err, ErrTrashNotFound)) {
			return QualityImportPlan{}, fmt.Errorf("default format groups: %w", err)
		}
		if err != nil {
			extra = nil // the guide publishes no groups for this app
		}
		if len(extra) > 0 {
			if qimp.FormatIDs == nil {
				qimp.FormatIDs = map[string]string{}
			}
			for name, id := range extra {
				if _, ok := qimp.FormatIDs[name]; !ok {
					qimp.FormatIDs[name] = id
				}
			}
		}
		found, err := s.deps.Guides.CustomFormats(ctx, app, qimp.FormatIDs)
		if err != nil {
			return QualityImportPlan{}, fmt.Errorf("custom formats: %w", err)
		}
		for _, id := range qimp.FormatIDs {
			if _, ok := found[id]; !ok {
				return QualityImportPlan{}, fmt.Errorf("custom format %q is missing from the guide", id)
			}
		}
		if qimp.App != "" {
			app = qimp.App
		}
		return PlanQualityImport(qimp, app, found, cfg), nil
	case ImportSourceDictionarry, ImportSourceProfilarr:
		if s.deps.Dictionarry == nil {
			return QualityImportPlan{}, errors.New("Profilarr (Dictionarry) is not configured")
		}
		qimp, found, err := s.deps.Dictionarry.Import(ctx, app, profileID)
		if err != nil {
			return QualityImportPlan{}, err
		}
		qimp.Source, qimp.ProfileID = ImportSourceDictionarry, profileID
		if qimp.App != "" {
			app = qimp.App
		}
		return PlanQualityImport(qimp, app, found, cfg), nil
	default:
		return QualityImportPlan{}, fmt.Errorf("unknown import source %q", imp.Source)
	}
}

// apply updates one saved profile in place and upserts the definitions its
// plan scores.
func (s *QualitySyncer) apply(c *config.Config, name, media string, saved core.ProfileImport, plan QualityImportPlan, now time.Time, report *QualitySyncReport) {
	idx := -1
	for i := range c.Quality.Profiles {
		if c.Quality.Profiles[i].Name == name && c.Quality.Profiles[i].MediaType == media {
			idx = i
			break
		}
	}
	if idx < 0 {
		// Renamed since the snapshot: identity still finds it.
		for i := range c.Quality.Profiles {
			p := c.Quality.Profiles[i]
			if p.MediaType == media && p.Import != nil && p.Import.Source == saved.Source && p.Import.App == saved.App && p.Import.Profile == saved.Profile {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return // deleted while fetching; nothing to overwrite
	}
	cur := c.Quality.Profiles[idx]
	// A same-name replacement during the fetch must not become guide-owned.
	if cur.Import == nil || cur.Import.Source != saved.Source || cur.Import.App != saved.App || cur.Import.Profile != saved.Profile {
		return
	}
	app := plan.App
	if !validApp(app) {
		app = saved.App
	}
	if !validApp(app) {
		return
	}
	next := plan.Profile
	next.Name = cur.Name
	next.MediaType = cur.MediaType
	if next.MediaType == "" {
		next.MediaType = core.MediaTypeForApp(app)
	}
	// Ranking and size policy are the owner's tuning, not part of the
	// guide's definition.
	next.ReleaseRules = cur.ReleaseRules
	next.Ranking = cur.Ranking
	next.RankingConfig = cur.RankingConfig
	next.EvaluationMode = cur.EvaluationMode
	next.PreferLargest = cur.PreferLargest
	next.FallbackBelowMin = cur.FallbackBelowMin
	next.MinSizeMBPerMin = cur.MinSizeMBPerMin
	next.MaxSizeMBPerMin = cur.MaxSizeMBPerMin
	next.EpisodeMinSizeMBPerMin = cur.EpisodeMinSizeMBPerMin
	next.EpisodeMaxSizeMBPerMin = cur.EpisodeMaxSizeMBPerMin
	next.AllowCam = cur.AllowCam
	next.CustomFormatScores = maps.Clone(plan.Profile.CustomFormatScores)
	if len(next.CustomFormatScores) == 0 {
		next.CustomFormatScores = nil
	}
	next.ConfiguredMaxHeight = 0
	next.SelectionKey = ""

	imported := core.ProfileImport{}
	if cur.Import != nil {
		imported = *cur.Import
	}
	imported.App = app
	imported.ScoreSet = plan.ScoreSet
	imported.SyncedAt = now
	if plan.TrashID != "" {
		imported.TrashID = plan.TrashID
	}
	if imported.Source == "" {
		imported.Source = plan.Source
	}
	if imported.Profile == "" {
		imported.Profile = plan.ProfileID
	}
	next.Import = &imported

	for _, def := range plan.Definitions {
		id := formatID(def)
		if id == "" {
			continue
		}
		if def.App == "" {
			def.App = app
		}
		if plan.Source != "" {
			def.ManagedBy = plan.Source + ":" + app
		}
		at := -1
		for i := range c.Resolver.CustomFormats {
			if formatID(c.Resolver.CustomFormats[i]) == id {
				at = i
				break
			}
		}
		if at < 0 {
			c.Resolver.CustomFormats = append(c.Resolver.CustomFormats, def)
			// Adding a guide format must not change any other profile's scores.
			for i := range c.Quality.Profiles {
				p := &c.Quality.Profiles[i]
				if _, exists := p.CustomFormatScores[id]; !exists {
					if p.CustomFormatScores == nil {
						p.CustomFormatScores = map[string]int{}
					}
					p.CustomFormatScores[id] = 0
				}
			}
			report.FormatsAdded++
			continue
		}
		if c.Resolver.CustomFormats[at].ManagedBy != "" && !sameFormatDefinition(c.Resolver.CustomFormats[at], def) {
			c.Resolver.CustomFormats[at] = def
			report.FormatsUpdated++
		}
	}
	// Imported profiles explicitly opt out of unrelated shared formats while
	// ordinary sparse owner maps retain their global-score inheritance.
	if next.CustomFormatScores == nil {
		next.CustomFormatScores = map[string]int{}
	}
	for _, def := range c.Resolver.CustomFormats {
		if id := formatID(def); id != "" {
			if _, exists := next.CustomFormatScores[id]; !exists {
				next.CustomFormatScores[id] = 0
			}
		}
	}
	if !sameProfileDefinition(cur, next) {
		report.ProfilesUpdated++
	}
	c.Quality.Profiles[idx] = next
}

// sameProfileDefinition compares the guide-owned and owner-tuned profile
// fields, ignoring provenance (SyncedAt changes on every run) and runtime
// fields. A sync with no real change must not report the profile updated.
func sameProfileDefinition(a, b core.QualityProfile) bool {
	a.Import, b.Import = nil, nil
	a.SelectionKey, b.SelectionKey = "", ""
	a.ConfiguredMaxHeight, b.ConfiguredMaxHeight = 0, 0
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	return err == nil && bytes.Equal(aj, bj)
}

// pruneManagedFormats removes guide-owned definitions no profile scores
// any more. A 0 score is what an import or manual save backfills for every
// other format, so only nonzero references count; desired holds the formats
// the synced plans still reference at any score. Manual definitions are
// never removed.
func pruneManagedFormats(c *config.Config, desired map[string]bool) int {
	used := map[string]bool{}
	for id := range desired {
		used[id] = true
	}
	for _, p := range c.Quality.Profiles {
		for id, score := range p.CustomFormatScores {
			if score != 0 {
				used[id] = true
			}
		}
	}
	kept := make([]core.CustomFormat, 0, len(c.Resolver.CustomFormats))
	removed := 0
	for _, f := range c.Resolver.CustomFormats {
		id := formatID(f)
		if f.ManagedBy != "" && !used[id] {
			removed++
			// Every reference is a backfilled 0; drop the dangling key so
			// the saved score maps only name formats that still exist.
			for i := range c.Quality.Profiles {
				delete(c.Quality.Profiles[i].CustomFormatScores, id)
			}
			continue
		}
		kept = append(kept, f)
	}
	c.Resolver.CustomFormats = kept
	return removed
}

func sameFormatDefinition(a, b core.CustomFormat) bool {
	return a.ID == b.ID && a.Name == b.Name && a.App == b.App && a.ManagedBy == b.ManagedBy &&
		maps.Equal(a.Scores, b.Scores) && sameSpecifications(a.Specifications, b.Specifications)
}

// sameSpecifications compares the semantics, not the raw JSON formatting:
// Fields holds json.RawMessage values, and a config save rewrites their
// whitespace, so reflect.DeepEqual reported a change on every sync.
func sameSpecifications(a, b []core.FormatSpecification) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		aj, err := json.Marshal(a[i])
		if err != nil {
			return false
		}
		bj, err := json.Marshal(b[i])
		if err != nil || !bytes.Equal(aj, bj) {
			return false
		}
	}
	return true
}

// syncReason is err as the owner should read it.
func syncReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the guide did not answer in time"
	case errors.Is(err, ErrTrashNotFound):
		return "the profile no longer exists in TRaSH Guides"
	case errors.Is(err, ErrDictionarryNotFound):
		return "the profile no longer exists in Profilarr (Dictionarry)"
	default:
		return err.Error()
	}
}

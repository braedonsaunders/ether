package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

func init() {
	store.RegisterViewerTable("catalog", "catalog_viewer_rules", "viewer_id", store.ViewerMergeMove)
	store.Register("catalog_viewer_rules", 1, `CREATE TABLE catalog_viewer_rules(viewer_id INTEGER PRIMARY KEY, document TEXT NOT NULL, revision INTEGER NOT NULL);`)
}

// ViewerRules keeps catalogue preferences separate from mandatory safety policy.
// Exemptions identify the exact global rule, so reordering cannot change them.
type ViewerRules struct {
	Mode              string                  `json:"mode"`
	Exclusions        []core.LibraryExclusion `json:"exclusions"`
	VisibilityDefault string                  `json:"visibilityDefault"`
	Exemptions        []string                `json:"exemptions"`
}
type compiledViewerRules struct {
	document       ViewerRules
	browse, search *libraryfilter.Filter
	revision       int64
}
type viewerRuleCache struct {
	sync.RWMutex
	entries map[int64]compiledViewerRules
	bus     *clusterbus.Invalidator
}

func RuleIdentity(rule core.LibraryExclusion) string {
	rule.Name = "" // Renaming a rule preserves the exemption.
	raw, _ := json.Marshal(rule)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func withMediaType(rule core.LibraryExclusion, mt string) core.LibraryExclusion {
	rule.MediaType = mt
	return rule
}

// IsRuleExempt checks whether rule is exempted by skip, matching both its own
// identity and legacy unscoped ("mixed" / "") identities from before rules
// moved onto individual Movies and TV Shows libraries.
func IsRuleExempt(skip map[string]bool, rule core.LibraryExclusion) bool {
	if len(skip) == 0 {
		return false
	}
	return skip[RuleIdentity(rule)] ||
		skip[RuleIdentity(withMediaType(rule, "mixed"))] ||
		skip[RuleIdentity(withMediaType(rule, ""))]
}

func compileViewerRulesDocument(p ViewerRules, pol LibraryPolicies) (*libraryfilter.Filter, error) {
	switch p.Mode {
	case "", "inherit":
		return libraryfilter.CompileLibraryPolicies(pol.MovieRules, pol.MovieDefault, pol.ShowRules, pol.ShowDefault)
	case "custom":
		return libraryfilter.CompilePolicy(p.Exclusions, p.VisibilityDefault)
	case "exempt":
		skip := map[string]bool{}
		for _, id := range p.Exemptions {
			skip[id] = true
		}
		movies := make([]core.LibraryExclusion, 0, len(pol.MovieRules))
		for _, rule := range pol.MovieRules {
			if !IsRuleExempt(skip, rule) {
				movies = append(movies, rule)
			}
		}
		shows := make([]core.LibraryExclusion, 0, len(pol.ShowRules))
		for _, rule := range pol.ShowRules {
			if !IsRuleExempt(skip, rule) {
				shows = append(shows, rule)
			}
		}
		return libraryfilter.CompileLibraryPolicies(movies, pol.MovieDefault, shows, pol.ShowDefault)
	default:
		return nil, fmt.Errorf("invalid catalogue rule mode")
	}
}

// recompileViewerRules follows a library policy change using the already-loaded
// documents. Policy callbacks must never wait for a database connection.
func (c *Catalog) recompileViewerRules(pol LibraryPolicies) error {
	c.viewerRules.Lock()
	defer c.viewerRules.Unlock()
	entries := make(map[int64]compiledViewerRules, len(c.viewerRules.entries))
	for id, v := range c.viewerRules.entries {
		f, err := c.compileViewerRules(v.document, pol)
		if err != nil {
			return err
		}
		v.browse, v.search = f, f.ForSearch()
		entries[id] = v
	}
	c.viewerRules.entries = entries
	return nil
}

func (c *Catalog) reloadViewerRules(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `SELECT viewer_id,document,revision FROM catalog_viewer_rules`)
	if err != nil {
		return err
	}
	defer rows.Close()
	pol := c.LibraryPolicies()
	entries := map[int64]compiledViewerRules{}
	for rows.Next() {
		var id int64
		var raw string
		var v compiledViewerRules
		if err := rows.Scan(&id, &raw, &v.revision); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &v.document); err != nil {
			return err
		}
		v.browse, err = c.compileViewerRules(v.document, pol)
		if err != nil {
			return err
		}
		v.search = v.browse.ForSearch()
		entries[id] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.viewerRules.Lock()
	c.viewerRules.entries = entries
	c.viewerRules.Unlock()
	return nil
}

// ReadViewerRulesInputs is the per-viewer rules source of a viewer-policy
// inputs rebuild: what every stored custom document reads. Inherit and exempt
// documents read a subset of the canonical library rules.
func ReadViewerRulesInputs(ctx context.Context, tx *sql.Tx) (viewerpolicy.Inputs, error) {
	rows, err := tx.QueryContext(ctx, `SELECT document FROM catalog_viewer_rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	in := viewerpolicy.Inputs{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var doc ViewerRules
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return nil, err
		}
		viewerpolicy.RulesInputs(in, doc.Exclusions)
	}
	return in, rows.Err()
}

// PolicyInputs lists what the installed canonical library rules read, and
// whether the installed config is expected.
func (c *Catalog) PolicyInputs(expected config.Catalog) (viewerpolicy.Inputs, bool) {
	if _, ok := c.CaptureConfigEvidence(expected); !ok {
		return nil, false
	}
	pol := c.LibraryPolicies()
	in := viewerpolicy.Inputs{}
	viewerpolicy.RulesInputs(in, pol.MovieRules)
	viewerpolicy.RulesInputs(in, pol.ShowRules)
	return in, true
}

func (c *Catalog) SetViewerRulesBus(bus clusterbus.Bus) {
	iv := clusterbus.NewInvalidator(bus, "catalog_viewer_rules", c.log, func(ctx context.Context, _ []string, _ bool) {
		c.configUpdateMu.Lock()
		defer c.configUpdateMu.Unlock()
		if err := c.reloadViewerRules(ctx); err != nil {
			c.log.Error("catalog: reload viewer rules", "err", err)
		}
	})
	c.viewerRules.Lock()
	old := c.viewerRules.bus
	c.viewerRules.bus = iv
	c.viewerRules.Unlock()
	old.Close()
}
func (c *Catalog) ViewerRules(id int64) ViewerRules {
	c.viewerRules.RLock()
	p, ok := c.viewerRules.entries[id]
	c.viewerRules.RUnlock()
	if !ok {
		return ViewerRules{Mode: "inherit", Exclusions: []core.LibraryExclusion{}, Exemptions: []string{}}
	}
	raw, _ := json.Marshal(p.document)
	var out ViewerRules
	_ = json.Unmarshal(raw, &out)
	if out.Exclusions == nil {
		out.Exclusions = []core.LibraryExclusion{}
	}
	if out.Exemptions == nil {
		out.Exemptions = []string{}
	}
	return out
}
func (c *Catalog) PutViewerRules(ctx context.Context, id int64, p ViewerRules) error {
	c.configUpdateMu.Lock()
	defer c.configUpdateMu.Unlock()
	if id <= 0 {
		return fmt.Errorf("invalid viewer")
	}
	pol := c.LibraryPolicies()
	if _, err := c.compileViewerRules(p, pol); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO catalog_viewer_rules(viewer_id,document,revision) VALUES(?,?,1) ON CONFLICT(viewer_id) DO UPDATE SET document=excluded.document,revision=catalog_viewer_rules.revision+1`, id, string(raw)); err != nil {
		return err
	}
	in := viewerpolicy.Inputs{}
	viewerpolicy.RulesInputs(in, p.Exclusions)
	if err = viewerpolicy.AddInputs(ctx, c.db, tx, viewerpolicy.SourceRules, in); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err := c.reloadViewerRules(ctx); err != nil {
		return err
	}
	c.viewerRules.RLock()
	iv := c.viewerRules.bus
	c.viewerRules.RUnlock()
	iv.Notify(strconv.FormatInt(id, 10))
	return nil
}
func (c *Catalog) viewerFilter(id int64, search bool) *libraryfilter.Filter {
	c.viewerRules.RLock()
	v, ok := c.viewerRules.entries[id]
	c.viewerRules.RUnlock()
	if ok && v.document.Mode != "inherit" && v.document.Mode != "" {
		if search {
			return v.search
		}
		return v.browse
	}
	if search {
		return c.searchFilter()
	}
	return c.visibilityFilter()
}

func (c *Catalog) hasCustomViewerRules(id int64) bool {
	if id == 0 {
		return false
	}
	c.viewerRules.RLock()
	v, ok := c.viewerRules.entries[id]
	c.viewerRules.RUnlock()
	return ok && v.document.Mode == "custom"
}

// strictViewer reports a viewer whose catalogue rules must fail closed on
// titles they cannot judge: one with their own ("custom") rule set, or a
// restricted one (a rating cap or section limits). The owner, inheriting and
// exempted profiles keep the fail-open behaviour.
func (c *Catalog) strictViewer(id int64, restricted bool) bool {
	if restricted {
		return true
	}
	return c.hasCustomViewerRules(id)
}

func userRestricted(u *core.User) bool {
	return u != nil && (u.MaxContentRating != "" || len(u.AllowedLibraries) > 0)
}

// undecided reports a title f cannot judge that the catalogue holds no
// details for: a remote summary, or a stored row whose details were never
// fetched. One indexed point read, only for titles that are undecidable.
func (c *Catalog) undecided(f *libraryfilter.Filter, t *core.Title) bool {
	if f.Passive() || f.Decidable(t) {
		return false
	}
	var detail int
	err := c.db.QueryRowContext(context.Background(), `SELECT detail FROM catalog_titles WHERE key=?`, int64(t.Key)).Scan(&detail)
	return err != nil || detail == 0
}

// strictVisible is visibleWith plus the fail-closed rule for strict viewers.
func (c *Catalog) strictVisible(f *libraryfilter.Filter, strict bool, t *core.Title) bool {
	if strict && c.undecided(f, t) {
		return false
	}
	return c.visibleWith(f, t)
}

func (c *Catalog) VisibleForViewer(u *core.User, t *core.Title) bool {
	var id int64
	if u != nil {
		id = u.ID
	}
	if t != nil && !c.hasCustomViewerRules(id) && c.isTitleOrShowOwnerIncluded(t) {
		return c.visibleWith(nil, t)
	}
	// Direct lookup and playback retain browsing-only titles.
	return c.strictVisible(c.viewerFilter(id, true), c.strictViewer(id, userRestricted(u)), t)
}
func (c *Catalog) viewerPaging(ctx context.Context, f core.Filter) core.Filter {
	f.CatalogCandidates = core.CatalogCandidates(ctx)
	if id := core.CatalogViewerID(ctx); id != 0 {
		f.CatalogViewerID = id
	}
	c.viewerRules.RLock()
	f.CatalogRuleRevision = c.viewerRules.entries[f.CatalogViewerID].revision
	c.viewerRules.RUnlock()
	return f
}

func (c *Catalog) ViewerRulesRevision(u *core.User) int64 {
	if u == nil {
		return 0
	}
	c.viewerRules.RLock()
	defer c.viewerRules.RUnlock()
	return c.viewerRules.entries[u.ID].revision
}

// ViewerRulesJudge is u's own rules revision and whether the rules that
// apply to u (their own, or the household's they inherit) judge titles on
// their facts, so that a catalogue write can change what u may see.
func (c *Catalog) ViewerRulesJudge(u *core.User) (int64, bool) {
	if u == nil {
		return 0, !c.visibilityFilter().Passive()
	}
	return c.ViewerRulesRevision(u), !c.viewerFilter(u.ID, false).Passive()
}

// ReadViewerRulesRevision is an indexed point read, also used by durable Home snapshots.
func ReadViewerRulesRevision(ctx context.Context, db *sql.DB, id int64) (int64, error) {
	var revision int64
	err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM catalog_viewer_rules WHERE viewer_id=?),0)`, id).Scan(&revision)
	return revision, err
}

func (c *Catalog) DiscoveryVisibleForViewer(u *core.User, t *core.Title) bool {
	var id int64
	if u != nil {
		id = u.ID
	}
	if t != nil && !c.hasCustomViewerRules(id) && c.isTitleOrShowOwnerIncluded(t) {
		return c.visibleWith(nil, t)
	}
	return c.strictVisible(c.viewerFilter(id, false), c.strictViewer(id, userRestricted(u)), t)
}

// RefreshViewerRules follows viewer removal or merge after the lifecycle transaction.
func (c *Catalog) RefreshViewerRules(ctx context.Context) error {
	c.configUpdateMu.Lock()
	defer c.configUpdateMu.Unlock()
	if err := c.reloadViewerRules(ctx); err != nil {
		return err
	}
	c.viewerRules.RLock()
	iv := c.viewerRules.bus
	c.viewerRules.RUnlock()
	iv.Notify("")
	return nil
}

func (c *Catalog) compileLibraryPolicies(p LibraryPolicies) (*libraryfilter.Filter, error) {
	f, err := libraryfilter.CompileLibraryPolicies(p.MovieRules, p.MovieDefault, p.ShowRules, p.ShowDefault)
	if err != nil {
		return nil, err
	}
	return c.bindLibraryRules(f)
}
func (c *Catalog) compileViewerRules(p ViewerRules, pol LibraryPolicies) (*libraryfilter.Filter, error) {
	f, err := compileViewerRulesDocument(p, pol)
	if err != nil {
		return nil, err
	}
	return c.bindLibraryRules(f)
}

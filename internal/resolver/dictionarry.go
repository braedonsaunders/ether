package resolver

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	_ "modernc.org/sqlite"
)

// Dictionarry (Profilarr v2) publishes its quality profiles, custom formats,
// conditions, and regular expressions in the Dictionarry-Hub/database v2
// repository as an ordered sequence of SQL operations against the
// Dictionarry-Hub/schema 1.1.0 relational schema.
const (
	dictionarryDefaultArchiveURL = "https://codeload.github.com/Dictionarry-Hub/database/tar.gz/refs/heads/v2"
	dictionarryArchiveLimit      = 16 << 20 // 16 MiB compressed archive bound
	dictionarryMaxTarEntries     = 2000     // maximum files in archive
	dictionarryMaxSQLBytes       = 32 << 20 // 32 MiB total uncompressed SQL bound
)

var (
	// ErrDictionarryNotFound reports a profile that Dictionarry does not list.
	ErrDictionarryNotFound = errors.New("Profilarr (Dictionarry) has no such profile")
	// ErrDictionarryUnavailable wraps failures to reach or compile Dictionarry.
	ErrDictionarryUnavailable = errors.New("Profilarr (Dictionarry) is unavailable")

	dictionarryUnsafeSQL = regexp.MustCompile(`(?i)\b(ATTACH|DETACH|PRAGMA|VACUUM|VIRTUAL|load_extension|readfile|writefile|fts3_tokenizer)\b`)
	dictionarryMdLink    = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
)

// DictionarryOptions configures a Dictionarry client. Zero values use GitHub.
type DictionarryOptions struct {
	ArchiveURL string // default https://codeload.github.com/Dictionarry-Hub/database/tar.gz/refs/heads/v2
	Client     *http.Client
	UserAgent  string
	TTL        time.Duration // cache lifetime, default 1h
	Now        func() time.Time
}

// Dictionarry reads and compiles Dictionarry v2 (Profilarr) quality profiles
// and custom formats, caching the compiled catalog in memory for TTL. When a
// refresh fails, the last good catalog is served instead.
type Dictionarry struct {
	o      DictionarryOptions
	mu     sync.Mutex
	cached *dictionarryCatalog
	at     time.Time
	flight *dictionarryFlight
}

type dictionarryFlight struct {
	done chan struct{}
	cat  *dictionarryCatalog
	err  error
}

type dictionarryCatalog struct {
	// profiles by app ("radarr" / "sonarr"), sorted for display.
	profiles map[string][]TrashProfile
	// imports by app -> profile ID ("2160p-remux") -> translated QualityImport.
	imports map[string]map[string]QualityImport
	// byName maps app -> lowercase profile name -> profile ID.
	byName map[string]map[string]string
	// formats by app -> format ID -> compiled CustomFormat definition.
	formats map[string]map[string]core.CustomFormat
}

// NewDictionarry returns a client for Dictionarry's published database.
func NewDictionarry(o DictionarryOptions) *Dictionarry {
	o.ArchiveURL = strings.TrimSpace(cmpOr(o.ArchiveURL, dictionarryDefaultArchiveURL))
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 30 * time.Second}
	}
	o.UserAgent = cmpOr(o.UserAgent, "Ether (+https://github.com/braedonsaunders/ether)")
	if o.TTL <= 0 {
		o.TTL = time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Dictionarry{o: o}
}

// Profiles lists app's Dictionarry quality profiles.
func (d *Dictionarry) Profiles(ctx context.Context, app string) ([]TrashProfile, error) {
	if !validApp(app) {
		return nil, fmt.Errorf("app must be radarr or sonarr")
	}
	cat, err := d.catalog(ctx)
	if err != nil {
		return nil, err
	}
	return slices.Clone(cat.profiles[app]), nil
}

// Import resolves a Dictionarry profile (by ID slug or exact profile name) and
// the custom formats it references for app.
func (d *Dictionarry) Import(ctx context.Context, app, id string) (QualityImport, map[string]core.CustomFormat, error) {
	if !validApp(app) {
		return QualityImport{}, nil, fmt.Errorf("app must be radarr or sonarr")
	}
	cat, err := d.catalog(ctx)
	if err != nil {
		return QualityImport{}, nil, err
	}
	key := strings.TrimSpace(id)
	if mapped, ok := cat.byName[app][strings.ToLower(key)]; ok {
		key = mapped
	}
	imp, ok := cat.imports[app][key]
	if !ok {
		return QualityImport{}, nil, ErrDictionarryNotFound
	}
	outImp := imp
	outImp.FormatIDs = maps.Clone(imp.FormatIDs)
	outImp.Skipped = slices.Clone(imp.Skipped)
	outImp.Warnings = slices.Clone(imp.Warnings)

	found := make(map[string]core.CustomFormat, len(imp.FormatIDs))
	appFormats := cat.formats[app]
	for _, fid := range imp.FormatIDs {
		if cf, ok := appFormats[fid]; ok {
			cp := cf
			cp.Specifications = slices.Clone(cf.Specifications)
			cp.Scores = maps.Clone(cf.Scores)
			if score, hasScore := cf.Scores[imp.Profile.Name]; hasScore {
				cp.Scores["default"] = score
			}
			found[fid] = cp
		}
	}
	return outImp, found, nil
}

func (d *Dictionarry) catalog(ctx context.Context) (*dictionarryCatalog, error) {
	d.mu.Lock()
	have := d.cached != nil
	if have && d.o.Now().Sub(d.at) < d.o.TTL {
		cat := d.cached
		d.mu.Unlock()
		return cat, nil
	}
	f := d.flight
	if f == nil {
		f = &dictionarryFlight{done: make(chan struct{})}
		d.flight = f
		go func() {
			lctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			f.cat, f.err = d.fetchAndCompile(lctx)
			d.mu.Lock()
			if f.err == nil && f.cat != nil {
				d.cached = f.cat
				d.at = d.o.Now()
			}
			d.flight = nil
			d.mu.Unlock()
			close(f.done)
		}()
	}
	d.mu.Unlock()

	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.err != nil {
		d.mu.Lock()
		stale := d.cached
		d.mu.Unlock()
		if stale != nil {
			return stale, nil
		}
		return nil, f.err
	}
	return f.cat, nil
}

func (d *Dictionarry) fetchAndCompile(ctx context.Context) (*dictionarryCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.o.ArchiveURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", d.o.UserAgent)
	resp, err := d.o.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: could not reach GitHub", ErrDictionarryUnavailable)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: database archive not found", ErrDictionarryUnavailable)
	case resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"):
		return nil, fmt.Errorf("%w: GitHub's request limit is used up", ErrDictionarryUnavailable)
	default:
		return nil, fmt.Errorf("%w: GitHub answered HTTP %d", ErrDictionarryUnavailable, resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, dictionarryArchiveLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: the download was interrupted", ErrDictionarryUnavailable)
	}
	if int64(len(raw)) > dictionarryArchiveLimit {
		return nil, fmt.Errorf("%w: archive exceeds size limit", ErrDictionarryUnavailable)
	}
	return compileDictionarryArchive(ctx, raw)
}

type dictionarryOp struct {
	seq  int
	name string
	sql  string
}

func compileDictionarryArchive(ctx context.Context, archiveBytes []byte) (*dictionarryCatalog, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid gzip archive", ErrDictionarryUnavailable)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var (
		ops       []dictionarryOp
		seenSeq   = map[int]string{}
		totalSQL  int64
		entries   int
		foundPCD  bool
		schemaDep string
	)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: corrupt tar archive", ErrDictionarryUnavailable)
		}
		entries++
		if entries > dictionarryMaxTarEntries {
			return nil, fmt.Errorf("%w: archive has too many entries", ErrDictionarryUnavailable)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		clean := path.Clean(strings.TrimLeft(hdr.Name, "/"))
		// Strip the top-level directory GitHub tarballs include ("database-v2/...").
		if _, rest, ok := strings.Cut(clean, "/"); ok {
			clean = rest
		}
		switch {
		case clean == "pcd.json":
			data, rerr := io.ReadAll(io.LimitReader(tr, (64<<10)+1))
			if rerr != nil || len(data) > 64<<10 {
				return nil, fmt.Errorf("%w: invalid pcd.json", ErrDictionarryUnavailable)
			}
			var meta struct {
				Dependencies map[string]string `json:"dependencies"`
			}
			if json.Unmarshal(data, &meta) != nil {
				return nil, fmt.Errorf("%w: invalid pcd.json metadata", ErrDictionarryUnavailable)
			}
			for k, v := range meta.Dependencies {
				if strings.HasSuffix(strings.ToLower(k), "dictionarry-hub/schema") {
					schemaDep = strings.TrimSpace(v)
				}
			}
			foundPCD = true
		case strings.HasPrefix(clean, "ops/") && strings.HasSuffix(clean, ".sql"):
			base := strings.TrimPrefix(clean, "ops/")
			if strings.Contains(base, "/") {
				continue
			}
			prefix, _, ok := strings.Cut(base, ".")
			if !ok {
				return nil, fmt.Errorf("%w: invalid operation file %q", ErrDictionarryUnavailable, base)
			}
			seq, perr := strconv.Atoi(prefix)
			if perr != nil || seq < 0 {
				return nil, fmt.Errorf("%w: invalid operation sequence in %q", ErrDictionarryUnavailable, base)
			}
			if prev, dup := seenSeq[seq]; dup {
				return nil, fmt.Errorf("%w: duplicate operation sequence %d (%s and %s)", ErrDictionarryUnavailable, seq, prev, base)
			}
			seenSeq[seq] = base
			if hdr.Size < 0 || totalSQL+hdr.Size > dictionarryMaxSQLBytes {
				return nil, fmt.Errorf("%w: SQL operations exceed size limit", ErrDictionarryUnavailable)
			}
			sqlBytes, rerr := io.ReadAll(io.LimitReader(tr, dictionarryMaxSQLBytes-totalSQL+1))
			if rerr != nil || int64(len(sqlBytes)) > dictionarryMaxSQLBytes-totalSQL {
				return nil, fmt.Errorf("%w: SQL operations exceed size limit", ErrDictionarryUnavailable)
			}
			totalSQL += int64(len(sqlBytes))
			sqlText := string(sqlBytes)
			if err := validateDictionarrySQL(base, sqlText); err != nil {
				return nil, err
			}
			ops = append(ops, dictionarryOp{seq: seq, name: base, sql: sqlText})
		}
	}

	if foundPCD && schemaDep != "" && !strings.HasPrefix(schemaDep, "1.") {
		return nil, fmt.Errorf("%w: unsupported Dictionarry schema version %q", ErrDictionarryUnavailable, schemaDep)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("%w: no SQL operations found in archive", ErrDictionarryUnavailable)
	}
	slices.SortFunc(ops, func(a, b dictionarryOp) int { return cmp.Compare(a.seq, b.seq) })

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("%w: sqlite open: %v", ErrDictionarryUnavailable, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, "PRAGMA trusted_schema = OFF; PRAGMA foreign_keys = ON;"); err != nil {
		return nil, fmt.Errorf("%w: sqlite pragma: %v", ErrDictionarryUnavailable, err)
	}
	if _, err := db.ExecContext(ctx, dictionarrySchemaSQL); err != nil {
		return nil, fmt.Errorf("%w: schema init: %v", ErrDictionarryUnavailable, err)
	}
	for _, op := range ops {
		if _, err := db.ExecContext(ctx, op.sql); err != nil {
			return nil, fmt.Errorf("%w: operation %s failed: %v", ErrDictionarryUnavailable, op.name, err)
		}
	}
	return extractDictionarryCatalog(ctx, db)
}

func validateDictionarrySQL(name, sqlText string) error {
	// Strip single-quoted string literals and SQL line comments before
	// checking for disallowed keywords.
	stripped := stripSQLStringsAndComments(sqlText)
	if m := dictionarryUnsafeSQL.FindString(stripped); m != "" {
		return fmt.Errorf("%w: disallowed SQL keyword %q in %s", ErrDictionarryUnavailable, m, name)
	}
	return nil
}

func stripSQLStringsAndComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inQuote = false
			}
			continue
		}
		if c == '\'' {
			inQuote = true
			b.WriteByte(' ')
			continue
		}
		if c == '-' && i+1 < len(s) && s[i+1] == '-' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

type dictionarryProfileRow struct {
	name            string
	description     string
	upgradesAllowed bool
	minScore        int
	cutoffScore     int
	incrementScore  int
	tags            []string
	languages       []dictionarryLangRule
}

type dictionarryLangRule struct {
	name string
	kind string // "simple", "must", "only", "not"
}

type dictionarryQualityItem struct {
	name    string
	group   bool
	members []string
}

func extractDictionarryCatalog(ctx context.Context, db *sql.DB) (*dictionarryCatalog, error) {
	// 1. Load quality_api_mappings: app -> canonical quality_name -> api_name.
	apiNames := map[string]map[string]string{
		AppRadarr: {},
		AppSonarr: {},
	}
	qRows, err := db.QueryContext(ctx, `SELECT quality_name, arr_type, api_name FROM quality_api_mappings`)
	if err != nil {
		return nil, fmt.Errorf("%w: query quality mappings: %v", ErrDictionarryUnavailable, err)
	}
	for qRows.Next() {
		var qn, app, api string
		if err := qRows.Scan(&qn, &app, &api); err != nil {
			qRows.Close()
			return nil, err
		}
		if m, ok := apiNames[app]; ok {
			m[qn] = api
		}
	}
	qRows.Close()

	// 2. Load profiles.
	pRows, err := db.QueryContext(ctx, `
		SELECT name, COALESCE(description, ''), upgrades_allowed, minimum_custom_format_score, upgrade_until_score, upgrade_score_increment
		FROM quality_profiles
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("%w: query profiles: %v", ErrDictionarryUnavailable, err)
	}
	var profiles []dictionarryProfileRow
	for pRows.Next() {
		var p dictionarryProfileRow
		var up int
		if err := pRows.Scan(&p.name, &p.description, &up, &p.minScore, &p.cutoffScore, &p.incrementScore); err != nil {
			pRows.Close()
			return nil, err
		}
		p.upgradesAllowed = up != 0
		profiles = append(profiles, p)
	}
	pRows.Close()
	if len(profiles) == 0 {
		return nil, fmt.Errorf("%w: no quality profiles found", ErrDictionarryUnavailable)
	}

	// 3. Load tags and language rules per profile.
	for i := range profiles {
		p := &profiles[i]
		tRows, err := db.QueryContext(ctx, `SELECT tag_name FROM quality_profile_tags WHERE quality_profile_name = ? ORDER BY tag_name`, p.name)
		if err == nil {
			for tRows.Next() {
				var tag string
				if tRows.Scan(&tag) == nil {
					p.tags = append(p.tags, tag)
				}
			}
			tRows.Close()
		}
		lRows, err := db.QueryContext(ctx, `SELECT language_name, type FROM quality_profile_languages WHERE quality_profile_name = ? ORDER BY language_name`, p.name)
		if err == nil {
			for lRows.Next() {
				var lr dictionarryLangRule
				if lRows.Scan(&lr.name, &lr.kind) == nil {
					p.languages = append(p.languages, lr)
				}
			}
			lRows.Close()
		}
	}

	// 4. Load custom format scores per profile and app:
	// profileScores[app][profileName][cfName] = score.
	// An app-specific row ('radarr' or 'sonarr') overrides an 'all' row.
	profileScores := map[string]map[string]map[string]int{
		AppRadarr: {},
		AppSonarr: {},
	}
	for _, p := range profiles {
		profileScores[AppRadarr][p.name] = map[string]int{}
		profileScores[AppSonarr][p.name] = map[string]int{}
	}
	sRows, err := db.QueryContext(ctx, `
		SELECT quality_profile_name, custom_format_name, arr_type, score
		FROM quality_profile_custom_formats
		ORDER BY CASE WHEN arr_type = 'all' THEN 0 ELSE 1 END
	`)
	if err != nil {
		return nil, fmt.Errorf("%w: query profile custom formats: %v", ErrDictionarryUnavailable, err)
	}
	for sRows.Next() {
		var profName, cfName, arrType string
		var score int
		if err := sRows.Scan(&profName, &cfName, &arrType, &score); err != nil {
			sRows.Close()
			return nil, err
		}
		switch arrType {
		case "all":
			if m, ok := profileScores[AppRadarr][profName]; ok {
				m[cfName] = score
			}
			if m, ok := profileScores[AppSonarr][profName]; ok {
				m[cfName] = score
			}
		case AppRadarr, AppSonarr:
			if m, ok := profileScores[arrType][profName]; ok {
				m[cfName] = score
			}
		}
	}
	sRows.Close()

	// 5. Compile custom formats for both apps.
	formats := map[string]map[string]core.CustomFormat{
		AppRadarr: {},
		AppSonarr: {},
	}
	for _, app := range []string{AppRadarr, AppSonarr} {
		cfMap, err := extractDictionarryFormats(ctx, db, app, profileScores[app])
		if err != nil {
			return nil, err
		}
		formats[app] = cfMap
	}

	// 6. Build per-app profile summaries and QualityImport structs.
	cat := &dictionarryCatalog{
		profiles: map[string][]TrashProfile{AppRadarr: {}, AppSonarr: {}},
		imports:  map[string]map[string]QualityImport{AppRadarr: {}, AppSonarr: {}},
		byName:   map[string]map[string]string{AppRadarr: {}, AppSonarr: {}},
		formats:  formats,
	}

	for _, p := range profiles {
		qItems, err := extractDictionarryQualities(ctx, db, p.name)
		if err != nil {
			return nil, err
		}
		id := trashSlug(p.name)
		if id == "" {
			continue
		}
		desc := cleanDictionarryDescription(p.description)
		group := dictionarryProfileGroup(p.name, p.tags)

		for _, app := range []string{AppRadarr, AppSonarr} {
			imp, ok := buildDictionarryImport(app, id, p, qItems, apiNames[app], profileScores[app][p.name])
			if !ok {
				continue
			}
			summary := TrashProfile{
				ID:          id,
				TrashID:     imp.TrashID,
				Name:        p.name,
				Description: desc,
				Group:       group,
				URL:         "https://dictionarry.dev",
				Formats:     len(imp.FormatIDs),
			}
			cat.profiles[app] = append(cat.profiles[app], summary)
			cat.imports[app][id] = imp
			cat.byName[app][strings.ToLower(p.name)] = id
			cat.byName[app][strings.ToLower(id)] = id
		}
	}

	for _, app := range []string{AppRadarr, AppSonarr} {
		slices.SortFunc(cat.profiles[app], func(a, b TrashProfile) int {
			return cmp.Or(cmp.Compare(a.Group, b.Group), strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)))
		})
	}
	return cat, nil
}

func extractDictionarryQualities(ctx context.Context, db *sql.DB, profileName string) ([]dictionarryQualityItem, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(quality_name, ''), COALESCE(quality_group_name, '')
		FROM quality_profile_qualities
		WHERE quality_profile_name = ? AND enabled = 1
		ORDER BY position
	`, profileName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []dictionarryQualityItem
	for rows.Next() {
		var qName, gName string
		if err := rows.Scan(&qName, &gName); err != nil {
			return nil, err
		}
		if gName != "" {
			items = append(items, dictionarryQualityItem{name: gName, group: true})
		} else if qName != "" {
			items = append(items, dictionarryQualityItem{name: qName})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range items {
		if !items[i].group {
			continue
		}
		mRows, err := db.QueryContext(ctx, `
			SELECT quality_name
			FROM quality_group_members
			WHERE quality_profile_name = ? AND quality_group_name = ?
			ORDER BY position, quality_name
		`, profileName, items[i].name)
		if err != nil {
			return nil, err
		}
		for mRows.Next() {
			var m string
			if mRows.Scan(&m) == nil {
				items[i].members = append(items[i].members, m)
			}
		}
		mRows.Close()
	}
	return items, nil
}

func buildDictionarryImport(
	app, id string,
	p dictionarryProfileRow,
	qItems []dictionarryQualityItem,
	apiMap map[string]string,
	scores map[string]int,
) (QualityImport, bool) {
	type starrItem struct {
		Name    string   `json:"name"`
		Allowed bool     `json:"allowed"`
		Items   []string `json:"items,omitempty"`
	}
	var items []starrItem
	for _, qi := range qItems {
		if qi.group {
			var members []string
			for _, m := range qi.members {
				if api := apiMap[m]; api != "" {
					members = append(members, api)
				}
			}
			if len(members) > 0 {
				items = append(items, starrItem{Name: qi.name, Allowed: true, Items: members})
			}
		} else if api := apiMap[qi.name]; api != "" {
			items = append(items, starrItem{Name: api, Allowed: true})
		}
	}
	if len(items) == 0 {
		return QualityImport{}, false
	}

	formatItems := make(map[string]string, len(scores))
	for cfName := range scores {
		formatItems[cfName] = dictionarryFormatID(app, cfName)
	}

	var lang string
	var excludedLangs []string
	for _, lr := range p.languages {
		if lr.kind == "not" {
			excludedLangs = append(excludedLangs, lr.name)
			continue
		}
		if lang == "" {
			lang = lr.name
		}
	}

	payload := map[string]any{
		"name":                  p.name,
		"trash_id":              "dictionarry-" + app + "-" + id,
		"upgradeAllowed":        p.upgradesAllowed,
		"minFormatScore":        p.minScore,
		"cutoffFormatScore":     p.cutoffScore,
		"minUpgradeFormatScore": p.incrementScore,
		"items":                 items,
		"formatItems":           formatItems,
	}
	if lang != "" && !strings.EqualFold(lang, "Any") && !strings.EqualFold(lang, "Unknown") {
		payload["language"] = lang
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return QualityImport{}, false
	}
	imp, err := ImportQualityProfile(raw)
	if err != nil {
		return QualityImport{}, false
	}
	imp.App = app

	if len(excludedLangs) > 0 {
		imp.Warnings = append(imp.Warnings, fmt.Sprintf("Excluded language rules (%s) are handled by custom format scoring rather than a hard profile filter.", strings.Join(excludedLangs, ", ")))
	}

	// Refine HDR, Dolby Vision, codec, and audio preferences from Dictionarry's
	// profile tags and custom format scores (e.g. 1080p Efficient and
	// 1080p Quality HDR allow HDR/DV and prefer HEVC at 1080p).
	hasTag := func(want string) bool {
		return slices.ContainsFunc(p.tags, func(t string) bool { return strings.EqualFold(t, want) })
	}
	if hasTag("HDR") || scores["HDR"] > 0 || scores["HDR10+"] > 0 {
		imp.Profile.AllowHDR = true
	}
	if scores["Dolby Vision"] > 0 {
		imp.Profile.AllowHDR = true
		imp.Profile.AllowDolbyVision = true
	}
	if (hasTag("h265") || hasTag("x265") || hasTag("HEVC")) && (!hasTag("h264") && !hasTag("x264") || hasTag("HDR") || imp.Profile.MaxHeight >= 2160) {
		imp.Profile.PreferredCodecs = []string{"hevc", "h264"}
	}
	if hasTag("Lossless Audio") {
		imp.Profile.PreferredAudio = []string{"truehd", "dts", "eac3", "ac3", "aac"}
	}
	return imp, true
}

func dictionarryFormatID(app, cfName string) string {
	return "dictionarry-" + app + "-" + trashSlug(cfName)
}

func extractDictionarryFormats(
	ctx context.Context,
	db *sql.DB,
	app string,
	profileScores map[string]map[string]int,
) (map[string]core.CustomFormat, error) {
	// Load all regex patterns once.
	regexes := map[string]string{}
	rRows, err := db.QueryContext(ctx, `SELECT name, pattern FROM regular_expressions`)
	if err != nil {
		return nil, fmt.Errorf("%w: query regexes: %v", ErrDictionarryUnavailable, err)
	}
	for rRows.Next() {
		var name, pat string
		if err := rRows.Scan(&name, &pat); err != nil {
			rRows.Close()
			return nil, err
		}
		regexes[name] = pat
	}
	rRows.Close()

	// Query all conditions for this app joined with their type-specific values.
	cRows, err := db.QueryContext(ctx, `
		SELECT
			cfc.custom_format_name,
			cfc.name,
			cfc.type,
			cfc.negate,
			cfc.required,
			COALESCE(cp.regular_expression_name, ''),
			COALESCE(cr.resolution, ''),
			COALESCE(cs.source, ''),
			COALESCE(cqm.quality_modifier, ''),
			csz.min_bytes,
			csz.max_bytes,
			COALESCE(cl.language_name, ''),
			COALESCE(cl.except_language, 0),
			COALESCE(cif.flag, ''),
			COALESCE(crt.release_type, ''),
			cy.min_year,
			cy.max_year
		FROM custom_format_conditions cfc
		LEFT JOIN condition_patterns cp
			ON cfc.custom_format_name = cp.custom_format_name AND cfc.name = cp.condition_name
		LEFT JOIN condition_resolutions cr
			ON cfc.custom_format_name = cr.custom_format_name AND cfc.name = cr.condition_name
		LEFT JOIN condition_sources cs
			ON cfc.custom_format_name = cs.custom_format_name AND cfc.name = cs.condition_name
		LEFT JOIN condition_quality_modifiers cqm
			ON cfc.custom_format_name = cqm.custom_format_name AND cfc.name = cqm.condition_name
		LEFT JOIN condition_sizes csz
			ON cfc.custom_format_name = csz.custom_format_name AND cfc.name = csz.condition_name
		LEFT JOIN condition_languages cl
			ON cfc.custom_format_name = cl.custom_format_name AND cfc.name = cl.condition_name
		LEFT JOIN condition_indexer_flags cif
			ON cfc.custom_format_name = cif.custom_format_name AND cfc.name = cif.condition_name
		LEFT JOIN condition_release_types crt
			ON cfc.custom_format_name = crt.custom_format_name AND cfc.name = crt.condition_name
		LEFT JOIN condition_years cy
			ON cfc.custom_format_name = cy.custom_format_name AND cfc.name = cy.condition_name
		WHERE cfc.arr_type IN ('all', ?)
		ORDER BY cfc.custom_format_name, cfc.id
	`, app)
	if err != nil {
		return nil, fmt.Errorf("%w: query conditions: %v", ErrDictionarryUnavailable, err)
	}
	defer cRows.Close()

	specsByCF := map[string][]core.FormatSpecification{}
	for cRows.Next() {
		var (
			cfName, condName, condType             string
			negate, required, exceptLang           int
			regexName, resolution, source, qualMod string
			minBytes, maxBytes                     sql.NullInt64
			langName, flag, relType                string
			minYear, maxYear                       sql.NullInt64
		)
		if err := cRows.Scan(
			&cfName, &condName, &condType, &negate, &required,
			&regexName, &resolution, &source, &qualMod,
			&minBytes, &maxBytes, &langName, &exceptLang,
			&flag, &relType, &minYear, &maxYear,
		); err != nil {
			return nil, err
		}
		spec := translateDictionarryCondition(
			app, condName, condType, negate != 0, required != 0,
			regexes[regexName], resolution, source, qualMod,
			minBytes, maxBytes, langName, exceptLang != 0,
			flag, relType, minYear, maxYear,
		)
		specsByCF[cfName] = append(specsByCF[cfName], spec)
	}
	if err := cRows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]core.CustomFormat, len(specsByCF))
	for cfName, specs := range specsByCF {
		fid := dictionarryFormatID(app, cfName)
		scores := map[string]int{}
		for profName, profMap := range profileScores {
			if sc, ok := profMap[cfName]; ok {
				scores[profName] = sc
				if _, hasDef := scores["default"]; !hasDef {
					scores["default"] = sc
				}
			}
		}
		cf := core.CustomFormat{
			ID:             fid,
			Name:           cfName,
			Scores:         scores,
			Specifications: specs,
		}
		if app == AppSonarr {
			cf.App = AppSonarr
		}
		out[fid] = cf
	}
	return out, nil
}

func translateDictionarryCondition(
	app, name, condType string,
	negate, required bool,
	pattern, resolution, source, qualMod string,
	minBytes, maxBytes sql.NullInt64,
	langName string, exceptLang bool,
	flag, relType string,
	minYear, maxYear sql.NullInt64,
) core.FormatSpecification {
	spec := core.FormatSpecification{
		Name:     name,
		Negate:   negate,
		Required: required,
		Fields:   map[string]json.RawMessage{},
	}
	switch strings.ToLower(strings.TrimSpace(condType)) {
	case "release_title":
		spec.Implementation = "ReleaseTitleSpecification"
		spec.Fields["value"] = rawJSON(pattern)
	case "release_group":
		spec.Implementation = "ReleaseGroupSpecification"
		spec.Fields["value"] = rawJSON(pattern)
	case "edition":
		spec.Implementation = "EditionSpecification"
		spec.Fields["value"] = rawJSON(pattern)
	case "resolution":
		spec.Implementation = "ResolutionSpecification"
		h, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(resolution)), "p"))
		if err != nil || h <= 0 {
			spec.Fields["value"] = rawJSON(resolution)
		} else {
			spec.Fields["value"] = rawJSON(h)
		}
	case "source":
		spec.Implementation = "SourceSpecification"
		if v, ok := dictionarrySourceEnum(app, source); ok {
			spec.Fields["value"] = rawJSON(v)
		} else {
			spec.Fields["value"] = rawJSON(-1)
		}
	case "quality_modifier":
		spec.Implementation = "QualityModifierSpecification"
		if v, ok := dictionarryModifierEnum(qualMod); ok {
			spec.Fields["value"] = rawJSON(v)
		} else {
			spec.Fields["value"] = rawJSON(-1)
		}
	case "size":
		spec.Implementation = "SizeSpecification"
		if minBytes.Valid {
			spec.Fields["min"] = rawJSON(float64(minBytes.Int64) / (1 << 30))
		}
		if maxBytes.Valid {
			spec.Fields["max"] = rawJSON(float64(maxBytes.Int64) / (1 << 30))
		}
	case "language":
		spec.Implementation = "LanguageSpecification"
		if v, ok := dictionarryLanguageEnum(langName); ok {
			spec.Fields["value"] = rawJSON(v)
		} else {
			spec.Fields["value"] = rawJSON(-999)
		}
		if exceptLang {
			spec.Fields["exceptLanguage"] = rawJSON(true)
		}
	case "indexer_flag":
		spec.Implementation = "IndexerFlagSpecification"
		spec.Fields["value"] = rawJSON(flag)
	case "release_type":
		spec.Implementation = "ReleaseTypeSpecification"
		spec.Fields["value"] = rawJSON(relType)
	case "year":
		spec.Implementation = "YearSpecification"
		if minYear.Valid {
			spec.Fields["min"] = rawJSON(minYear.Int64)
		}
		if maxYear.Valid {
			spec.Fields["max"] = rawJSON(maxYear.Int64)
		}
	default:
		spec.Implementation = condType
	}
	return spec
}

func dictionarrySourceEnum(app, src string) (int, bool) {
	s := strings.ToLower(strings.TrimSpace(src))
	if app == AppSonarr {
		switch s {
		case "television", "tv", "hdtv":
			return 1, true
		case "television_raw", "raw_hd", "rawhd":
			return 2, true
		case "web_dl", "webdl", "web":
			return 3, true
		case "webrip":
			return 4, true
		case "dvd":
			return 5, true
		case "bluray":
			return 6, true
		case "bluray_raw", "remux":
			return 7, true
		}
		return 0, false
	}
	switch s {
	case "cam":
		return 1, true
	case "telesync", "ts":
		return 2, true
	case "telecine", "tc":
		return 3, true
	case "workprint":
		return 4, true
	case "dvd":
		return 5, true
	case "television", "tv", "hdtv":
		return 6, true
	case "web_dl", "webdl", "web":
		return 7, true
	case "webrip":
		return 8, true
	case "bluray", "bluray_raw":
		return 9, true
	}
	return 0, false
}

func dictionarryModifierEnum(mod string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(mod)) {
	case "none":
		return 0, true
	case "regional":
		return 1, true
	case "screener", "dvdscr":
		return 2, true
	case "rawhd", "raw_hd":
		return 3, true
	case "brdisk", "br_disk":
		return 4, true
	case "remux":
		return 5, true
	}
	return 0, false
}

func dictionarryLanguageEnum(name string) (int, bool) {
	switch n := strings.ToLower(strings.TrimSpace(name)); n {
	case "any":
		return languageAny, true
	case "original":
		return languageOriginal, true
	case "unknown":
		return 0, true
	default:
		for id, lang := range starrLanguages {
			if lang == n {
				return id, true
			}
		}
		return 0, false
	}
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func dictionarryProfileGroup(name string, tags []string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "2160p") || slices.Contains(tags, "2160p"):
		return 1
	case strings.Contains(lower, "1080p") || slices.Contains(tags, "1080p"):
		return 2
	case strings.Contains(lower, "720p") || slices.Contains(tags, "720p"):
		return 3
	default:
		return 4
	}
}

func cleanDictionarryDescription(s string) string {
	s = dictionarryMdLink.ReplaceAllString(s, "$1")
	s = strings.ReplaceAll(s, "**", "")
	return plainText(s)
}

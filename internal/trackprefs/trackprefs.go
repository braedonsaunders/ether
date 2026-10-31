// Package trackprefs persists viewer stream policy and show-specific choices.
package trackprefs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("trackprefs", 1, `CREATE TABLE trackprefs_rules(user_id INTEGER PRIMARY KEY, rules TEXT NOT NULL); CREATE TABLE trackprefs_memory(user_id INTEGER NOT NULL, show_id INTEGER NOT NULL, kind TEXT NOT NULL, choice TEXT NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY(user_id,show_id,kind));`)
}

// User zero is the owner-configured fallback policy; positive IDs are overrides.
type Rules struct {
	AudioLanguages     []string        `json:"audioLanguages"`
	SubtitleLanguages  []string        `json:"subtitleLanguages"`
	PreferOriginal     bool            `json:"preferOriginal"`
	SubtitleMode       string          `json:"subtitleMode"`  // off, forced-only, foreign-only, always
	SubtitleStyle      string          `json:"subtitleStyle"` // normal, sdh, forced
	SubtitleAppearance json.RawMessage `json:"subtitleAppearance,omitempty"`
	Loudness           bool            `json:"loudness"`
	NightMode          bool            `json:"nightMode"`
	DialogueBoost      bool            `json:"dialogueBoost"`
	MaxBitrateKbps     int             `json:"maxBitrateKbps,omitempty"` // 0 means no viewer preference
	MaxHeight          int             `json:"maxHeight,omitempty"`
	PreferredQuality   string          `json:"preferredQuality,omitempty"`
}
type Choice struct {
	Language string `json:"language"`
	Forced   bool   `json:"forced"`
	SDH      bool   `json:"sdh"`
	Off      bool   `json:"off"`
}
type Selection struct {
	Audio    int
	Subtitle int
}
type Options struct {
	ShowName func(context.Context, int64) string
	// SyncSubtitleLanguages mirrors the ordered list onto auth_users.subtitle_languages.
	// User 0 is the shared default and is not an account row.
	SyncSubtitleLanguages func(context.Context, int64, []string) error
	// ServerLanguage is the catalog metadata language (config catalog.language,
	// for example en-US). It is not the console locale.
	ServerLanguage func() string
}
type rulesCacheKey struct {
	user                    int64
	device, series, library string
}

type Service struct {
	db      *sql.DB
	options Options
	mu      sync.Mutex
	// rulesCache remembers merged rules until a preference write. Playback
	// still applies the rules on every plan; it does not re-read them.
	rulesCache map[rulesCacheKey]cachedRules
}

type cachedRules struct {
	rules   Rules
	sources map[string]string
}

func (s *Service) forgetRules() {
	s.mu.Lock()
	s.rulesCache = nil
	s.mu.Unlock()
}

func cloneRules(r Rules) Rules {
	r.AudioLanguages = append([]string(nil), r.AudioLanguages...)
	r.SubtitleLanguages = append([]string(nil), r.SubtitleLanguages...)
	r.SubtitleAppearance = append(json.RawMessage(nil), r.SubtitleAppearance...)
	return r
}

func New(db *sql.DB, opts ...Options) *Service {
	s := &Service{db: db}
	if len(opts) > 0 {
		s.options = opts[0]
	}
	return s
}
func Default() Rules {
	return Rules{AudioLanguages: []string{}, SubtitleLanguages: []string{}, SubtitleMode: "off", SubtitleStyle: "normal"}
}
func (r Rules) Validate() error {
	if r.MaxBitrateKbps < 0 || r.MaxHeight < 0 {
		return errors.New("invalid bitrate preference")
	}
	if r.PreferredQuality != "" {
		values := map[string]int{"auto": 0, "original": 0, "480p": 480, "720p": 720, "1080p": 1080, "2160p": 2160}
		if height, ok := values[r.PreferredQuality]; !ok || height != r.MaxHeight {
			return errors.New("invalid quality preference")
		}
	}
	switch r.SubtitleMode {
	case "off", "forced-only", "foreign-only", "always":
	default:
		return errors.New("invalid subtitle mode")
	}
	switch r.SubtitleStyle {
	case "normal", "sdh", "forced":
	default:
		return errors.New("invalid subtitle style")
	}
	if len(r.AudioLanguages) > 20 || len(r.SubtitleLanguages) > 20 {
		return errors.New("too many languages")
	}
	for _, lang := range append(append([]string{}, r.AudioLanguages...), r.SubtitleLanguages...) {
		if len(lang) < 2 || len(lang) > 15 || strings.ContainsAny(lang, " /\\\t\r\n") {
			return errors.New("invalid language code")
		}
	}
	return validateAppearance(r.SubtitleAppearance)
}
func (s *Service) PutRules(ctx context.Context, user int64, r Rules) error {
	if user < 0 {
		return errors.New("invalid user")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO trackprefs_rules(user_id,rules) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET rules=excluded.rules`, user, string(b))
	if err != nil {
		return err
	}
	s.forgetRules()
	return s.syncLanguages(ctx, user, r.SubtitleLanguages)
}

// HasViewerRules reports whether this viewer stored their own rules. GetRules
// also falls back to user 0, the shared default, which is not a change this
// viewer made.
func (s *Service) HasViewerRules(ctx context.Context, user int64) (bool, error) {
	if user <= 0 {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trackprefs_rules WHERE user_id=?`, user).Scan(&n)
	return n > 0, err
}

func (s *Service) GetRules(ctx context.Context, user int64) (Rules, error) {
	r := Default()
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT rules FROM trackprefs_rules WHERE user_id IN(0,?) ORDER BY user_id DESC LIMIT 1`, user).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	err = json.Unmarshal([]byte(raw), &r)
	if r.AudioLanguages == nil {
		r.AudioLanguages = []string{}
	}
	if r.SubtitleLanguages == nil {
		r.SubtitleLanguages = []string{}
	}
	return r, err
}
func (s *Service) DeleteRules(ctx context.Context, user int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM trackprefs_rules WHERE user_id=?`, user)
	if err == nil {
		s.forgetRules()
	}
	return err
}

// memoryTarget is the row id for a remembered track. Episodes share one row
// per show. Movies keep their own row, under the rating key, so a movie id
// cannot collide with a show id and a per-title choice is not discarded.
func memoryTarget(key core.RatingKey) (int64, bool) {
	switch key.Kind() {
	case core.KindEpisode:
		id := key.TMDB()
		return id, id > 0
	case core.KindMovie:
		return int64(key), key.Valid()
	default:
		return 0, false
	}
}

func (s *Service) Remember(ctx context.Context, user int64, key core.RatingKey, kind string, track *core.StreamInfo) error {
	target, ok := memoryTarget(key)
	if !ok {
		return nil
	}
	if user <= 0 || (kind != "audio" && kind != "subtitle") {
		return errors.New("invalid stream memory")
	}
	if track == nil && kind == "audio" {
		return nil
	}
	c := Choice{Off: track == nil}
	if track != nil {
		if track.Type != kind {
			return errors.New("stream type does not match")
		}
		c.Language = langtag.Normalize(track.Language)
		c.Forced = track.Forced
		c.SDH = sdh(*track)
	}
	b, _ := json.Marshal(c)
	_, err := s.db.ExecContext(ctx, `INSERT INTO trackprefs_memory VALUES(?,?,?,?,?) ON CONFLICT(user_id,show_id,kind) DO UPDATE SET choice=excluded.choice,updated_at=excluded.updated_at`, user, target, kind, string(b), time.Now().Unix())
	return err
}
func (s *Service) Forget(ctx context.Context, user, show int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM trackprefs_memory WHERE user_id=? AND show_id=?`, user, show)
	return err
}
func (s *Service) choice(ctx context.Context, user int64, key core.RatingKey, kind string) (Choice, bool, error) {
	var c Choice
	target, ok := memoryTarget(key)
	if !ok {
		return c, false, nil
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT choice FROM trackprefs_memory WHERE user_id=? AND show_id=? AND kind=?`, user, target, kind).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	err = json.Unmarshal([]byte(raw), &c)
	return c, err == nil, err
}
func sdh(t core.StreamInfo) bool {
	v := strings.ToLower(t.Title)
	return strings.Contains(v, "sdh") || strings.Contains(v, "hearing impaired") || strings.Contains(v, "[cc]") || v == "cc"
}

// rankLanguage orders configured language preferences first, match tier
// second, and leaves space for pick's style/default bonuses last. With at
// most 20 configured languages plus the original language, the score stays
// below the 10000-point explicit-choice bonus.
func rankLanguage(code string, want []string) int {
	for i, v := range want {
		if tier := langtag.MatchTier(v, code); tier > 0 {
			return (len(want)-i)*300 + tier*50
		}
	}
	return 0
}

func pick(streams []core.StreamInfo, kind string, want []string, style string, forcedOnly bool, memory *Choice) int {
	best, score := -1, -100000
	for i, t := range streams {
		if t.Type != kind || (forcedOnly && !t.Forced) {
			continue
		}
		n := rankLanguage(t.Language, want)
		if t.Default {
			n += 5
		}
		if kind == "subtitle" {
			if (style == "sdh") == sdh(t) {
				n += 10
			}
			if (style == "forced") == t.Forced {
				n += 10
			}
		}
		if memory != nil && langtag.Normalize(t.Language) == langtag.Normalize(memory.Language) && t.Forced == memory.Forced && sdh(t) == memory.SDH {
			n += 10000
		}
		if n > score {
			best, score = i, n
		}
	}
	return best
}

// Select returns positions in MediaInfo.Streams, never ffprobe indices.
func (s *Service) Select(ctx context.Context, user int64, key core.RatingKey, info core.MediaInfo, original string) (Selection, error) {
	out := Selection{-1, -1}
	r, err := s.GetRules(ctx, user)
	if err != nil {
		return out, err
	}
	return s.selectRules(ctx, user, key, info, original, r)
}

// SelectScoped falls back to Select unchanged when no scoped preference applies.
func (s *Service) SelectScoped(ctx context.Context, user int64, key core.RatingKey, info core.MediaInfo, original string, scope Scope) (Selection, bool, error) {
	rules, changed, err := s.ScopedRules(ctx, user, scope)
	if err != nil {
		return Selection{-1, -1}, false, err
	}
	if !changed {
		return Selection{}, false, nil
	}
	selection, err := s.selectRules(ctx, user, key, info, original, rules)
	return selection, true, err
}

func (s *Service) selectRules(ctx context.Context, user int64, key core.RatingKey, info core.MediaInfo, original string, r Rules) (Selection, error) {
	out := Selection{-1, -1}
	a, hasA, err := s.choice(ctx, user, key, "audio")
	if err != nil {
		return out, err
	}
	b, hasB, err := s.choice(ctx, user, key, "subtitle")
	if err != nil {
		return out, err
	}
	r = s.sharedLanguages(ctx, user, r)
	server := s.serverLanguage()
	want := AudioOrder(r.AudioLanguages, server, original, r.PreferOriginal)
	var am *Choice
	if hasA {
		am = &a
	}
	out.Audio = pick(info.Streams, "audio", want, "", false, am)
	if hasB && b.Off {
		return out, nil
	}
	understood := viewerLanguages(r.AudioLanguages, server)
	foreign := out.Audio >= 0 && len(understood) > 0 && rankLanguage(info.Streams[out.Audio].Language, understood) == 0
	if !hasB && r.SubtitleMode == "off" && !foreign {
		return out, nil
	}
	if !hasB && r.SubtitleMode == "foreign-only" {
		known := r.SubtitleLanguages
		if len(known) == 0 {
			known = r.AudioLanguages
		}
		if len(known) == 0 {
			known = viewerLanguages(nil, server)
		}
		if len(known) == 0 || (out.Audio >= 0 && rankLanguage(info.Streams[out.Audio].Language, known) > 0) {
			return out, nil
		}
	}
	var bm *Choice
	if hasB {
		bm = &b
	}
	style := r.SubtitleStyle
	if !hasB && r.SubtitleMode == "off" && foreign {
		style = "forced"
	}
	out.Subtitle = pick(info.Streams, "subtitle", subtitleOrder(r, server, original), style, !hasB && r.SubtitleMode == "forced-only", bm)
	return out, nil
}

// sharedLanguages fills an empty viewer list from user 0. GetRules already
// returns user 0 when the viewer has no row; a viewer row with a blank list
// would otherwise hide that shared language. PreferOriginal is left alone:
// it ranks the title original first and is not this chain.
func (s *Service) sharedLanguages(ctx context.Context, user int64, r Rules) Rules {
	if user <= 0 || (len(r.AudioLanguages) > 0 && len(r.SubtitleLanguages) > 0) {
		return r
	}
	shared, err := s.GetRules(ctx, 0)
	if err != nil {
		return r
	}
	if len(r.AudioLanguages) == 0 {
		r.AudioLanguages = append([]string(nil), shared.AudioLanguages...)
	}
	if len(r.SubtitleLanguages) == 0 {
		r.SubtitleLanguages = append([]string(nil), shared.SubtitleLanguages...)
	}
	return r
}

func (s *Service) serverLanguage() string {
	if s == nil || s.options.ServerLanguage == nil {
		return ""
	}
	return strings.TrimSpace(s.options.ServerLanguage())
}

// AudioOrder is the audio preference for one playback. audio is the viewer's
// list, or user 0's list when the viewer saved none. PreferOriginal ranks the
// title original first. Otherwise the order is that list, the catalog
// language, then the title original. pick uses the file default flag only
// when none of these match.
func AudioOrder(audio []string, server, original string, preferOriginal bool) []string {
	var want []string
	if preferOriginal {
		want = appendLanguage(want, original)
	}
	for _, lang := range audio {
		want = appendLanguage(want, lang)
	}
	want = appendLanguage(want, server)
	if !preferOriginal {
		want = appendLanguage(want, original)
	}
	return want
}

func subtitleOrder(r Rules, server, original string) []string {
	if len(r.SubtitleLanguages) > 0 {
		return AudioOrder(r.SubtitleLanguages, server, original, false)
	}
	return AudioOrder(r.AudioLanguages, server, original, r.PreferOriginal)
}

// viewerLanguages is the language the viewer is assumed to understand.
// The title's original language is not one of them.
func viewerLanguages(audio []string, server string) []string {
	if len(audio) > 0 {
		return append([]string(nil), audio...)
	}
	if server != "" {
		return []string{server}
	}
	return nil
}

func appendLanguage(have []string, lang string) []string {
	lang = strings.TrimSpace(lang)
	if lang == "" {
		return have
	}
	key := langtag.Normalize(lang)
	for _, v := range have {
		if key != "" && langtag.Normalize(v) == key {
			return have
		}
		if key == "" && strings.EqualFold(strings.TrimSpace(v), lang) {
			return have
		}
	}
	return append(have, lang)
}

// AudioFilters returns only known ffmpeg filters; arbitrary client strings never enter the command.
func (r Rules) AudioFilters(channels int) []string {
	var f []string
	if r.DialogueBoost && channels >= 6 {
		f = append(f, "pan=stereo|FL=0.7*FL+0.7*FC|FR=0.7*FR+0.7*FC")
	}
	if r.NightMode {
		f = append(f, "acompressor=threshold=0.125:ratio=4:attack=20:release=250:makeup=2")
	}
	if r.Loudness {
		f = append(f, "loudnorm=I=-16:TP=-1.5:LRA=11")
	}
	return f
}

type SavedChoice struct {
	ShowID    int64  `json:"showId"`
	ShowTitle string `json:"showTitle,omitempty"`
	Kind      string `json:"kind"`
	Choice    Choice `json:"choice"`
	UpdatedAt int64  `json:"updatedAt"`
}

func (s *Service) Memory(ctx context.Context, user int64) ([]SavedChoice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT show_id,kind,choice,updated_at FROM trackprefs_memory WHERE user_id=? ORDER BY show_id,kind`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavedChoice{}
	for rows.Next() {
		var v SavedChoice
		var raw string
		if err = rows.Scan(&v.ShowID, &v.Kind, &raw, &v.UpdatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &v.Choice); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if s.options.ShowName != nil {
		for i := range out {
			out[i].ShowTitle = s.options.ShowName(ctx, out[i].ShowID)
		}
	}
	return out, nil
}

// SetSubtitlePreferences updates only subtitle fields, preserving concurrent
// audio and other rule changes. The compare-and-swap is shared across nodes.
func (s *Service) SetSubtitlePreferences(ctx context.Context, user int64, languages []string, mode string) error {
	return s.updateViewerRules(ctx, user, func(r *Rules) {
		r.SubtitleLanguages = append([]string(nil), languages...)
		r.SubtitleMode = mode
	})
}

func (s *Service) SetAudioPreferences(ctx context.Context, user int64, languages []string) error {
	return s.updateViewerRules(ctx, user, func(r *Rules) { r.AudioLanguages = append([]string(nil), languages...) })
}

func (s *Service) SetSubtitleLanguage(ctx context.Context, user int64, languages []string) error {
	return s.updateViewerRules(ctx, user, func(r *Rules) { r.SubtitleLanguages = append([]string(nil), languages...) })
}

func (s *Service) SetSubtitleMode(ctx context.Context, user int64, mode string) error {
	return s.updateViewerRules(ctx, user, func(r *Rules) { r.SubtitleMode = mode })
}

func (s *Service) SetAppearance(ctx context.Context, user int64, raw json.RawMessage) error {
	if err := validateAppearance(raw); err != nil {
		return err
	}
	if len(raw) == 0 || string(raw) == "null" {
		raw = nil
	}
	stored := append(json.RawMessage(nil), raw...)
	return s.updateViewerRules(ctx, user, func(r *Rules) { r.SubtitleAppearance = stored })
}

func (s *Service) SetBitratePreference(ctx context.Context, user int64, kbps int) error {
	return s.updateViewerRules(ctx, user, func(r *Rules) { r.MaxBitrateKbps = kbps })
}

func (s *Service) SetHeightPreference(ctx context.Context, user int64, height int, quality string) error {
	return s.updateViewerRules(ctx, user, func(r *Rules) { r.MaxHeight = height; r.PreferredQuality = quality })
}

func (s *Service) updateViewerRules(ctx context.Context, user int64, update func(*Rules)) error {
	if user <= 0 {
		return errors.New("invalid user")
	}
	for attempt := 0; attempt < 16; attempt++ {
		var old string
		err := s.db.QueryRowContext(ctx, `SELECT rules FROM trackprefs_rules WHERE user_id=?`, user).Scan(&old)
		exists := err == nil
		var rules Rules
		if errors.Is(err, sql.ErrNoRows) {
			rules, err = s.GetRules(ctx, user)
		} else if err == nil {
			rules = Default()
			err = json.Unmarshal([]byte(old), &rules)
		}
		if err != nil {
			return err
		}
		update(&rules)
		if err = rules.Validate(); err != nil {
			return err
		}
		data, err := json.Marshal(rules)
		if err != nil {
			return err
		}
		var res sql.Result
		if exists {
			res, err = s.db.ExecContext(ctx, `UPDATE trackprefs_rules SET rules=? WHERE user_id=? AND rules=? AND EXISTS(SELECT 1 FROM auth_users WHERE id=? AND active=1)`, string(data), user, old, user)
		} else {
			res, err = s.db.ExecContext(ctx, `INSERT INTO trackprefs_rules(user_id,rules) SELECT ?,? WHERE EXISTS(SELECT 1 FROM auth_users WHERE id=? AND active=1) ON CONFLICT(user_id) DO NOTHING`, user, string(data), user)
		}
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			s.forgetRules()
			return s.syncLanguages(ctx, user, rules.SubtitleLanguages)
		}
		var active int
		if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users WHERE id=? AND active=1`, user).Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			return core.ErrUnauthorized
		}
	}
	return errors.New("trackprefs: concurrent update retry limit")
}

func (s *Service) syncLanguages(ctx context.Context, user int64, languages []string) error {
	if user <= 0 || s.options.SyncSubtitleLanguages == nil {
		return nil
	}
	return s.options.SyncSubtitleLanguages(ctx, user, append([]string(nil), languages...))
}

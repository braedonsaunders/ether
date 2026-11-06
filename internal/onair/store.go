package onair

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/store"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

func init() {
	// Media facts live with the asset so every node sees them. duration_ms is
	// NULL until probed; 0 means the asset has no duration (or could not be read).
	store.Register("onair", 4, `ALTER TABLE onair_assets ADD COLUMN duration_ms INTEGER;
 ALTER TABLE onair_assets ADD COLUMN width INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE onair_assets ADD COLUMN height INTEGER NOT NULL DEFAULT 0;`)
	store.Register("onair", 3, `CREATE TABLE onair_imports(id TEXT PRIMARY KEY,reel_id TEXT NOT NULL,data TEXT NOT NULL);
 ALTER TABLE onair_assets ADD COLUMN source TEXT NOT NULL DEFAULT '';
 ALTER TABLE onair_assets ADD COLUMN attribution TEXT NOT NULL DEFAULT '';`)
	store.Register("onair", 1, `
CREATE TABLE onair_looks(id TEXT PRIMARY KEY,data TEXT NOT NULL,pack_id TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL);
CREATE TABLE onair_families(id TEXT PRIMARY KEY,data TEXT NOT NULL,pack_id TEXT NOT NULL);
CREATE TABLE onair_assets(digest TEXT PRIMARY KEY,kind TEXT NOT NULL,name TEXT NOT NULL,mime TEXT NOT NULL,size INTEGER NOT NULL,license TEXT NOT NULL,content BLOB NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE onair_look_assets(look_id TEXT NOT NULL REFERENCES onair_looks(id) ON DELETE CASCADE,digest TEXT NOT NULL REFERENCES onair_assets(digest),PRIMARY KEY(look_id,digest));
CREATE TABLE onair_reels(id TEXT PRIMARY KEY,data TEXT NOT NULL,pack_id TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL);
CREATE TABLE onair_reel_clips(id TEXT PRIMARY KEY,reel_id TEXT NOT NULL REFERENCES onair_reels(id) ON DELETE CASCADE,name TEXT NOT NULL,duration_ms INTEGER NOT NULL,source_digest TEXT NOT NULL REFERENCES onair_assets(digest),status TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,data TEXT NOT NULL);
CREATE INDEX onair_reel_clips_reel ON onair_reel_clips(reel_id);
CREATE TABLE onair_copy(title_key INTEGER NOT NULL,language TEXT NOT NULL,kind TEXT NOT NULL,text TEXT NOT NULL,created_at INTEGER NOT NULL,PRIMARY KEY(title_key,language,kind));
CREATE TABLE onair_settings(k TEXT PRIMARY KEY,v TEXT NOT NULL);
`)
}

var ErrNotFound = errors.New("onair: not found")
var ErrUsed = errors.New("onair: asset is in use")

func newID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func normalizeFont(s string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(s))
}
func (s *Service) validateLook(ctx context.Context, l Look) error {
	if l.Screens != nil {
		for _, v := range []string{l.Screens.OffAir, l.Screens.NoSignal} {
			if !strings.Contains(" card static bars brb testcard error ", " "+v+" ") {
				return fmt.Errorf("unknown screen style %q", v)
			}
		}
	}

	if e := l.Bug.Validate(); e != nil {
		return e
	}
	if strings.TrimSpace(l.Name) == "" || utf8.RuneCountInString(l.Name) > 120 || utf8.RuneCountInString(l.Brand.Tagline) > 40 {
		return fmt.Errorf("name required (120 characters maximum); tagline maximum 40 characters")
	}
	known := " inter playfairdisplay specialelite fraunces bungee fredoka orbitron vt323 spacegrotesk ibmplexmono archivoblack notosans "
	for _, f := range []string{l.Brand.Type.Display, l.Brand.Type.Body} {
		if !strings.Contains(known, " "+normalizeFont(f)+" ") {
			var n int
			if e := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM onair_assets WHERE digest=? AND kind='font'", f).Scan(&n); e != nil || n == 0 {
				return fmt.Errorf("unknown font %q", f)
			}
		}
	}
	lum := func(v string) (float64, error) {
		if len(v) != 7 || v[0] != '#' {
			return 0, fmt.Errorf("palette must use #RRGGBB")
		}
		b, e := hex.DecodeString(v[1:])
		if e != nil {
			return 0, e
		}
		x := make([]float64, 3)
		for i, n := range b {
			x[i] = float64(n) / 255
			if x[i] <= .04045 {
				x[i] /= 12.92
			} else {
				x[i] = math.Pow((x[i]+.055)/1.055, 2.4)
			}
		}
		return .2126*x[0] + .7152*x[1] + .0722*x[2], nil
	}
	bg, e := lum(l.Brand.Palette.Background)
	if e != nil {
		return e
	}
	fg, e := lum(l.Brand.Palette.Text)
	if e != nil {
		return e
	}
	for _, v := range []string{l.Brand.Palette.Accent, l.Brand.Palette.Glow} {
		if _, e = lum(v); e != nil {
			return e
		}
	}
	if (math.Max(bg, fg)+.05)/(math.Min(bg, fg)+.05) < 4.5 {
		return fmt.Errorf("text contrast must be at least 4.5:1")
	}
	if l.Brand.Energy < 0 || l.Brand.Energy > 1 {
		return fmt.Errorf("energy must be between 0 and 1")
	}
	return s.validateScenes(ctx, l)
}
func (s *Service) Looks(ctx context.Context) ([]Look, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT data FROM onair_looks ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Look{}
	for rows.Next() {
		var b string
		var l Look
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &l); e != nil {
			return nil, e
		}
		l.emptyCollections()
		s.mu.Lock()
		p := s.progress[l.ID]
		if p != nil {
			v := *p
			l.Render = &v
		}
		s.mu.Unlock()
		out = append(out, l)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	rows.Close()
	if s.o.StyleChannels != nil {
		usage, err := s.o.StyleChannels(ctx)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].ChannelIDs = append([]string{}, usage[out[i].ID]...)
			out[i].ChannelCount = len(out[i].ChannelIDs)
		}
	}
	return out, nil
}
func (s *Service) Look(ctx context.Context, id string) (Look, error) {
	var b string
	var l Look
	e := s.db.QueryRowContext(ctx, "SELECT data FROM onair_looks WHERE id=?", id).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	if e == nil {
		e = json.Unmarshal([]byte(b), &l)
		l.emptyCollections()
	}
	if e == nil && s.o.StyleChannels != nil {
		usage, err := s.o.StyleChannels(ctx)
		if err != nil {
			return l, err
		}
		l.ChannelIDs = append([]string{}, usage[l.ID]...)
		l.ChannelCount = len(l.ChannelIDs)
	}
	return l, e
}
func (s *Service) SaveLook(ctx context.Context, l Look) (Look, error) {
	saved, err := s.saveLook(ctx, l)
	if err == nil && s.o.Changed != nil {
		err = s.o.Changed(ctx)
	}
	return saved, err
}
func (s *Service) saveLook(ctx context.Context, l Look) (Look, error) {
	if e := s.validateLook(ctx, l); e != nil {
		return l, e
	}
	if l.ID == "" {
		l.ID = newID()
	}
	if l.ChannelIDs == nil {
		l.ChannelIDs = []string{}
	}
	l.Render = nil
	l.Pack = nil
	b, e := json.Marshal(l)
	if e != nil {
		return l, e
	}
	s.assetMu.Lock()
	defer s.assetMu.Unlock()

	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return l, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "INSERT INTO onair_looks(id,data,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data,updated_at=excluded.updated_at", l.ID, string(b), time.Now().Unix()); e != nil {
		return l, e
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM onair_look_assets WHERE look_id=?", l.ID); e != nil {
		return l, e
	}
	re := regexp.MustCompile(`"([a-f0-9]{64})"`)
	for _, m := range re.FindAllSubmatch(b, -1) {
		if _, e = tx.ExecContext(ctx, "INSERT INTO onair_look_assets(look_id,digest) VALUES(?,?) ON CONFLICT(look_id,digest) DO NOTHING", l.ID, string(m[1])); e != nil {
			return l, fmt.Errorf("referenced asset missing: %w", e)
		}
	}
	e = tx.Commit()

	if e == nil {
		s.mu.Lock()
		s.looks[l.ID] = l
		s.mu.Unlock()
		s.enqueueLook(l)
	}
	return l, e
}
func (s *Service) DeleteLook(ctx context.Context, id string) error {
	_, e := s.db.ExecContext(ctx, "DELETE FROM onair_looks WHERE id=?", id)
	if e == nil {
		s.mu.Lock()
		delete(s.looks, id)
		delete(s.progress, id)
		delete(s.lookJobs, id)
		s.mu.Unlock()
	}
	if e == nil && s.o.Changed != nil {
		e = s.o.Changed(ctx)
	}
	return e
}
func (s *Service) seed(ctx context.Context) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, "INSERT INTO onair_settings(k,v) VALUES('seeded','1') ON CONFLICT(k) DO NOTHING")
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return tx.Commit()
	}
	for _, v := range [][10]string{{"nocturne", "Nocturne", "lateNight", "#060508", "#C8102E", "#EDE6D6", "#8FA3B8", "Playfair Display", "Special Elite", "Late night · lights off"}, {"meridian", "Meridian", "cinema", "#070d1d", "#D8B26E", "#F4EEDF", "#17254a", "Fraunces", "Inter", "A world of cinema"}, {"blorp", "BLORP!", "kids", "#6B2BD9", "#FF7A00", "#ffffff", "#7CE03A", "Bungee", "Fredoka", "Totally gooey TV"}, {"neon", "NEON 84", "retro", "#12002b", "#ff4fd8", "#ffffff", "#22e6ff", "Orbitron", "VT323", "Stay tuned"}, {"personal", "Your TV", "personal", "#100b23", "#9b85ff", "#ffffff", "#ff5ca8", "Space Grotesk", "Inter", "Made for you"}} {
		l := familyLook(v[2], v[1])
		l.ID = v[0]
		l.Brand.Palette.Background = v[3]
		l.Brand.Palette.Accent = v[4]
		l.Brand.Palette.Text = v[5]
		l.Brand.Palette.Glow = v[6]
		l.Brand.Type.Display = v[7]
		l.Brand.Type.Body = v[8]
		l.Brand.Tagline = v[9]
		l.Brand.Energy = .5
		l.Brand.Language = "en"
		l.Brand.Textures = []string{"Grain"}
		b, _ := json.Marshal(l)
		if _, e = tx.ExecContext(ctx, "INSERT INTO onair_looks(id,data,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING", l.ID, string(b), time.Now().Unix()); e != nil {
			return e
		}
	}
	return tx.Commit()
}

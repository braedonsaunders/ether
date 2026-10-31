// Package langfeat runs the owner's language-model features that a viewer
// or admin asks for explicitly: translating an existing subtitle, writing
// subtitles from audio, and translating an overview and tagline.
// Nothing here runs on a timer, and a search or a library row never calls
// a model.
package langfeat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/subtitles"
)

const (
	// TranslateOfferID and TranscribeOfferID are reserved subtitle offer
	// ids. They sit above ordinary provider file ids and inside the range
	// JavaScript can round-trip, because some clients keep the id in a number.
	TranslateOfferID  int64 = 9000000000000001
	TranscribeOfferID int64 = 9000000000000002

	offerTTL = time.Hour
)

// Gates are the switches and whether each requirement is configured.
type Gates struct {
	SubtitleTranslate bool
	SpeechToText      bool
	OverviewTranslate bool
	TextReady         bool
	SpeechReady       bool
}

// Offer is one subtitle choice shown in a viewer's search. Building the
// list does not call a model.
type Offer struct {
	ID       int64
	Language string
	Title    string
	Provider string
}

// Options build a Service.
type Options struct {
	Dir   string
	Gates func() Gates
	// Translate is one language-model call. system and user are the prompt.
	Translate func(ctx context.Context, system, user string) (string, error)
	// Transcribe reads a 16 kHz mono wav and returns timed subtitle text.
	Transcribe func(ctx context.Context, wavPath, language string) (string, error)
	Subtitles  *subtitles.Service
	// Audio extracts a wav of the playing title. It runs only when a viewer
	// picks the from-audio track.
	Audio func(ctx context.Context, user *core.User, title *core.Title) (wavPath string, cleanup func(), err error)
	// SourceText reads an embedded subtitle when nothing is cached yet.
	// It runs only when a viewer picks the translated track.
	SourceText func(ctx context.Context, user *core.User, title *core.Title, exceptLang string) (lang, text string, err error)
	Now        func() time.Time
}

// Service caches translations and transcripts under Dir.
type Service struct {
	o  Options
	mu sync.Mutex
	// pending remembers the language a search offered, so the later pick
	// knows what the viewer asked for.
	pending   map[pendingKey]pendingOffer
	gateCache Gates
	gatesAt   time.Time
}

type pendingKey struct {
	title core.RatingKey
	id    int64
}

type pendingOffer struct {
	lang string
	at   time.Time
}

type overviewDoc struct {
	Summary string `json:"summary"`
	Tagline string `json:"tagline"`
}

// New builds a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Gates == nil {
		o.Gates = func() Gates { return Gates{} }
	}
	return &Service{o: o, pending: map[pendingKey]pendingOffer{}}
}

// ResetGates drops the short-lived copy of Gates. Tests use it after a save.
func (s *Service) ResetGates() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.gatesAt = time.Time{}
	s.mu.Unlock()
}

func (s *Service) gates() Gates {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gatesAt.IsZero() && s.o.Now().Sub(s.gatesAt) < 2*time.Second {
		return s.gateCache
	}
	s.gateCache = s.o.Gates()
	s.gatesAt = s.o.Now()
	return s.gateCache
}

func (s *Service) note(title core.RatingKey, id int64, lang string) {
	now := s.o.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 1024 {
		for k, v := range s.pending {
			if now.Sub(v.at) >= offerTTL {
				delete(s.pending, k)
			}
		}
	}
	s.pending[pendingKey{title, id}] = pendingOffer{lang: lang, at: now}
}

func (s *Service) noted(title core.RatingKey, id int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.pending[pendingKey{title, id}]
	if !ok || s.o.Now().Sub(v.at) >= offerTTL {
		return "", false
	}
	return v.lang, true
}

// IsReserved reports whether id belongs to a language feature offer.
func IsReserved(id int64) bool {
	return id == TranslateOfferID || id == TranscribeOfferID
}

// Offers lists the tracks a viewer may ask for. It does not translate,
// transcribe, or read the media.
func (s *Service) Offers(title *core.Title, lang string) []Offer {
	if s == nil || title == nil {
		return nil
	}
	lang = langtag.Normalize(lang)
	if lang == "" {
		return nil
	}
	g := s.gates()
	var out []Offer
	if g.SubtitleTranslate && g.TextReady {
		s.note(title.Key, TranslateOfferID, lang)
		out = append(out, Offer{
			ID: TranslateOfferID, Language: lang,
			Title: "Translated from the existing subtitles", Provider: "Ether",
		})
	}
	if g.SpeechToText && g.SpeechReady {
		s.note(title.Key, TranscribeOfferID, lang)
		out = append(out, Offer{
			ID: TranscribeOfferID, Language: lang,
			Title: "Created from the audio", Provider: "Ether",
		})
	}
	return out
}

// Fulfill builds the track a viewer picked. A cached track is returned
// without calling a model.
func (s *Service) Fulfill(ctx context.Context, user *core.User, title *core.Title, id int64) (core.StreamInfo, error) {
	if s == nil || title == nil || s.o.Subtitles == nil {
		return core.StreamInfo{}, core.ErrNotFound
	}
	if st, ok := s.o.Subtitles.StreamByFileID(title, id); ok {
		return st, nil
	}
	lang, ok := s.noted(title.Key, id)
	if !ok {
		return core.StreamInfo{}, core.ErrNotFound
	}
	switch id {
	case TranslateOfferID:
		return s.translateTrack(ctx, user, title, lang)
	case TranscribeOfferID:
		return s.transcribeTrack(ctx, user, title, lang)
	default:
		return core.StreamInfo{}, core.ErrNotFound
	}
}

func (s *Service) translateTrack(ctx context.Context, user *core.User, title *core.Title, lang string) (core.StreamInfo, error) {
	if s.o.Translate == nil {
		return core.StreamInfo{}, errors.New("language model is off")
	}
	from, text, ok := s.o.Subtitles.SourceText(title.Key, lang)
	if !ok && s.o.SourceText != nil {
		var err error
		from, text, err = s.o.SourceText(ctx, user, title, lang)
		if err != nil {
			return core.StreamInfo{}, err
		}
		ok = strings.TrimSpace(text) != ""
	}
	if !ok {
		return core.StreamInfo{}, errors.New("this title has no subtitle track to translate yet")
	}
	cues, err := subtitles.ParseCues(text)
	if err != nil {
		return core.StreamInfo{}, err
	}
	translated, err := translateCues(ctx, s.o.Translate, cues, from, lang)
	if err != nil {
		return core.StreamInfo{}, err
	}
	return s.o.Subtitles.StoreText(title, lang, "Translated from the existing subtitles", subtitles.RenderSRT(translated), TranslateOfferID)
}

func (s *Service) transcribeTrack(ctx context.Context, user *core.User, title *core.Title, lang string) (core.StreamInfo, error) {
	if s.o.Transcribe == nil || s.o.Audio == nil {
		return core.StreamInfo{}, errors.New("speech-to-text is not set up")
	}
	wav, cleanup, err := s.o.Audio(ctx, user, title)
	if err != nil {
		return core.StreamInfo{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	text, err := s.o.Transcribe(ctx, wav, lang)
	if err != nil {
		return core.StreamInfo{}, err
	}
	if _, err := subtitles.ParseCues(text); err != nil {
		return core.StreamInfo{}, err
	}
	return s.o.Subtitles.StoreText(title, lang, "Created from the audio", text, TranscribeOfferID)
}

// Overview returns a cached translation. When detail is true and nothing
// is cached, it translates. A library row passes detail false and never
// calls the model.
func (s *Service) Overview(ctx context.Context, title *core.Title, lang string, detail bool) (summary, tagline string, ok bool) {
	if s == nil || title == nil {
		return "", "", false
	}
	g := s.gates()
	if !g.OverviewTranslate || !g.TextReady {
		return "", "", false
	}
	lang = langtag.Normalize(lang)
	if lang == "" || lang == langtag.Normalize(title.Language) {
		return "", "", false
	}
	if summary == "" && strings.TrimSpace(title.Summary) == "" && strings.TrimSpace(title.Tagline) == "" {
		return "", "", false
	}
	if doc, hit := s.readOverview(title.Key, lang); hit {
		return doc.Summary, doc.Tagline, true
	}
	if !detail || s.o.Translate == nil {
		return "", "", false
	}
	doc, err := s.writeOverview(ctx, title, lang)
	if err != nil {
		return "", "", false
	}
	return doc.Summary, doc.Tagline, true
}

// Queue translates one title for an admin. A cached result is returned
// without another model call.
func (s *Service) Queue(ctx context.Context, title *core.Title, lang string) (overviewDoc, error) {
	if s == nil || title == nil {
		return overviewDoc{}, errors.New("missing title")
	}
	g := s.gates()
	if !g.OverviewTranslate {
		return overviewDoc{}, errors.New("turn on description translation first")
	}
	if !g.TextReady || s.o.Translate == nil {
		return overviewDoc{}, errors.New("connect a language model first")
	}
	lang = langtag.Normalize(lang)
	if lang == "" {
		return overviewDoc{}, errors.New("use a language code such as fr")
	}
	if lang == langtag.Normalize(title.Language) {
		return overviewDoc{}, errors.New("that title is already in this language")
	}
	if strings.TrimSpace(title.Summary) == "" && strings.TrimSpace(title.Tagline) == "" {
		return overviewDoc{}, errors.New("this title has no overview or tagline to translate")
	}
	if doc, hit := s.readOverview(title.Key, lang); hit {
		return doc, nil
	}
	return s.writeOverview(ctx, title, lang)
}

func (s *Service) writeOverview(ctx context.Context, title *core.Title, lang string) (overviewDoc, error) {
	in, _ := json.Marshal(overviewDoc{Summary: title.Summary, Tagline: title.Tagline})
	system := "You translate catalogue descriptions. Reply with JSON {\"overview\":\"\",\"tagline\":\"\"} only. Do not add facts. Keep people's names. If a field is empty, return it empty."
	user := fmt.Sprintf("Translate into %s.\n%s", lang, in)
	raw, err := s.o.Translate(ctx, system, user)
	if err != nil {
		return overviewDoc{}, err
	}
	var parsed struct {
		Overview string `json:"overview"`
		Tagline  string `json:"tagline"`
		Summary  string `json:"summary"`
	}
	if err := json.Unmarshal(extractJSON(raw), &parsed); err != nil {
		return overviewDoc{}, fmt.Errorf("language model did not return a translation")
	}
	doc := overviewDoc{Summary: clip(parsed.Overview, 8000), Tagline: clip(parsed.Tagline, 500)}
	if doc.Summary == "" {
		doc.Summary = clip(parsed.Summary, 8000)
	}
	if doc.Summary == "" && doc.Tagline == "" {
		return overviewDoc{}, fmt.Errorf("language model returned an empty translation")
	}
	if err := s.saveOverview(title.Key, lang, doc); err != nil {
		return overviewDoc{}, err
	}
	return doc, nil
}

func (s *Service) overviewPath(key core.RatingKey, lang string) string {
	return filepath.Join(s.o.Dir, fmt.Sprintf("%d.%s.json", int64(key), lang))
}

func (s *Service) readOverview(key core.RatingKey, lang string) (overviewDoc, bool) {
	if s.o.Dir == "" {
		return overviewDoc{}, false
	}
	b, err := os.ReadFile(s.overviewPath(key, lang))
	if err != nil || len(b) == 0 || len(b) > 64<<10 {
		return overviewDoc{}, false
	}
	var doc overviewDoc
	if json.Unmarshal(b, &doc) != nil {
		return overviewDoc{}, false
	}
	if doc.Summary == "" && doc.Tagline == "" {
		return overviewDoc{}, false
	}
	return doc, true
}

func (s *Service) saveOverview(key core.RatingKey, lang string, doc overviewDoc) error {
	if s.o.Dir == "" {
		return fmt.Errorf("translation cache unavailable")
	}
	if err := os.MkdirAll(s.o.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(s.overviewPath(key, lang), b, 0o644)
}

func translateCues(ctx context.Context, tr func(context.Context, string, string) (string, error), cues []subtitles.Cue, from, to string) ([]subtitles.Cue, error) {
	const batch = 40
	out := append([]subtitles.Cue(nil), cues...)
	system := "You translate subtitle cue text. The user message contains a JSON array of strings. Reply with a JSON array of the same length and order. Do not add timestamps, numbers, or commentary. Leave a line unchanged when it is only a sound effect."
	for i := 0; i < len(cues); i += batch {
		j := i + batch
		if j > len(cues) {
			j = len(cues)
		}
		lines := make([]string, j-i)
		for k := i; k < j; k++ {
			lines[k-i] = cues[k].Text
		}
		payload, _ := json.Marshal(lines)
		user := fmt.Sprintf("Translate into %s. Source language: %s.\n%s", to, from, payload)
		raw, err := tr(ctx, system, user)
		if err != nil {
			return nil, err
		}
		var parsed []string
		if json.Unmarshal(extractJSON(raw), &parsed) != nil || len(parsed) != len(lines) {
			var wrapped struct {
				Lines []string `json:"lines"`
			}
			if json.Unmarshal(extractJSON(raw), &wrapped) != nil || len(wrapped.Lines) != len(lines) {
				return nil, fmt.Errorf("translation returned %d lines, want %d", len(parsed), len(lines))
			}
			parsed = wrapped.Lines
		}
		for k, line := range parsed {
			line = strings.TrimSpace(line)
			if line == "" {
				line = lines[k]
			}
			out[i+k].Text = line
		}
	}
	return out, nil
}

func extractJSON(s string) []byte {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	start := strings.IndexAny(s, "[{")
	end := strings.LastIndexAny(s, "]}")
	if start < 0 || end < start {
		return []byte(s)
	}
	return []byte(s[start : end+1])
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

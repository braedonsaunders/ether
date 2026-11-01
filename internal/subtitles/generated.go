package subtitles

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
)

// StoreText saves subtitle text the server produced (a translation or a
// transcript) and remembers it as the viewer's pick for that language.
// A later call with the same title, language and file ID returns the cached
// track and does not rewrite it. It never contacts a provider.
func (s *Service) StoreText(title *core.Title, lang, label, text string, fileID int64) (core.StreamInfo, error) {
	if title == nil || s == nil || s.dir == "" {
		return core.StreamInfo{}, fmt.Errorf("subtitle cache unavailable")
	}
	lang = langtag.Normalize(lang)
	if lang == "" {
		return core.StreamInfo{}, fmt.Errorf("subtitle language is required")
	}
	if fileID == 0 {
		return core.StreamInfo{}, fmt.Errorf("subtitle file id is required")
	}
	id := cacheID(title, lang, fileID)
	if !(s.cached(id, "srt") && s.cached(id, "vtt")) {
		srt, vtt, err := NormalizeLang([]byte(text), lang)
		if err != nil {
			return core.StreamInfo{}, err
		}
		if err := os.MkdirAll(s.dir, 0o755); err != nil {
			return core.StreamInfo{}, err
		}
		meta, _ := json.Marshal(map[string]any{
			"titleKey": title.Key.String(), "fileId": fileID, "language": lang,
			"release": label, "fetchedAt": s.now().UTC(), "generated": true,
		})
		for _, w := range []struct{ name, data string }{
			{id + ".json", string(meta)},
			{id + ".vtt", vtt},
			{id + ".srt", srt},
		} {
			if err := writeFileAtomic(filepath.Join(s.dir, w.name), []byte(w.data)); err != nil {
				return core.StreamInfo{}, err
			}
		}
	}
	sel := selection{ID: id, FileID: fileID, Language: lang, Release: label, At: s.now().UTC()}
	p := s.manualPath(title.Key, lang)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return core.StreamInfo{}, err
	}
	b, err := json.Marshal(sel)
	if err != nil {
		return core.StreamInfo{}, err
	}
	s.pickMu.Lock()
	err = writeFileAtomic(p, b)
	s.pickMu.Unlock()
	if err != nil {
		return core.StreamInfo{}, err
	}
	return sidecarStream(0, lang, sel), nil
}

// StreamByFileID returns a generated or picked sidecar already on disk.
// It does not download anything.
func (s *Service) StreamByFileID(title *core.Title, fileID int64) (core.StreamInfo, bool) {
	if s == nil || title == nil || s.dir == "" || fileID == 0 {
		return core.StreamInfo{}, false
	}
	for _, e := range s.titleSelections(title.Key) {
		if e.sel.FileID == fileID {
			return sidecarStream(0, e.lang, e.sel), true
		}
	}
	return core.StreamInfo{}, false
}

// SourceText returns one cached subtitle in a language other than except,
// preferring English. It reads only files already on disk.
func (s *Service) SourceText(key core.RatingKey, except string) (lang, text string, ok bool) {
	if s == nil || s.dir == "" {
		return "", "", false
	}
	except = langtag.Normalize(except)
	var fallbackLang, fallback string
	for _, e := range s.titleSelections(key) {
		if e.lang == except {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.sel.ID+".srt"))
		if err != nil || len(b) == 0 || len(b) > 2<<20 {
			continue
		}
		if e.lang == "en" {
			return "en", string(b), true
		}
		if fallback == "" {
			fallbackLang, fallback = e.lang, string(b)
		}
	}
	if fallback == "" {
		return "", "", false
	}
	return fallbackLang, fallback, true
}

// ManualSidecars lists viewer-picked tracks already on disk. Unlike
// CachedSidecars it does not require OpenSubtitles to be configured, so a
// translated or transcribed track still appears when that account is off.
func (s *Service) ManualSidecars(title *core.Title, embedded []core.StreamInfo) []core.StreamInfo {
	if s == nil || title == nil || s.dir == "" {
		return nil
	}
	present := map[string]bool{}
	next := 1
	for _, st := range embedded {
		if st.External && st.ExternalURL != "" {
			present[st.ExternalURL] = true
		}
		if st.Index >= next {
			next = st.Index + 1
		}
	}
	manual := s.manualSelections(title.Key)
	var out []core.StreamInfo
	add := func(sel selection, lang string) {
		if present[URLPrefix+sel.ID] {
			return
		}
		present[URLPrefix+sel.ID] = true
		out = append(out, sidecarStream(next, lang, sel))
		next++
	}
	for _, lang := range sortedLanguages(manual) {
		add(manual[lang], lang)
	}
	for _, e := range s.titleSelections(title.Key) {
		if e.sig == uploadSig {
			add(e.sel, e.lang)
		}
	}
	return out
}

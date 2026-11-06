package subtitles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
)

// DeleteUpload removes only the title/language upload that issued this track.
// Known index paths keep the mutation independent of the library's size.
func (s *Service) DeleteUpload(key core.RatingKey, stream core.StreamInfo) error {
	lang := langtag.Normalize(stream.Language)
	if !stream.External || lang == "" || !strings.HasPrefix(stream.ExternalURL, URLPrefix) {
		return core.ErrNotFound
	}
	id := strings.TrimPrefix(stream.ExternalURL, URLPrefix)
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return core.ErrNotFound
	}
	s.pickMu.Lock()
	defer s.pickMu.Unlock()
	index := s.uploadPath(key, lang)
	raw, err := os.ReadFile(index)
	if err != nil {
		return core.ErrNotFound
	}
	var sel selection
	if json.Unmarshal(raw, &sel) != nil || sel.ID != id {
		return core.ErrNotFound
	}
	if err := os.Remove(index); err != nil {
		return err
	}
	for _, ext := range []string{".srt", ".vtt", ".json"} {
		if err := os.Remove(filepath.Join(s.dir, id+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

package subtitles

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langtag"
)

const (
	uploadReleasePrefix = "Uploaded: "
	maxUploadLabel      = 128
)

// AcceptsUploadFormat is the Plex Web upload format check: case-sensitive
// on the wire. PMS accepts srt/ass/ssa/smi/txt/psb; Ether also accepts vtt.
func AcceptsUploadFormat(format string) bool {
	switch format {
	case "srt", "vtt", "ass", "ssa", "smi", "txt", "psb":
		return true
	}
	return false
}

// StoreUpload saves a viewer-uploaded sidecar as UTF-8 SRT+VTT. It is
// listed as an upload-sig track and does not overwrite another viewer's
// manual pick.
func (s *Service) StoreUpload(title *core.Title, filename, format, language string, body []byte) (core.StreamInfo, error) {
	if s == nil || title == nil || s.dir == "" {
		return core.StreamInfo{}, fmt.Errorf("subtitle cache unavailable")
	}
	if len(body) == 0 {
		return core.StreamInfo{}, fmt.Errorf("empty subtitle")
	}
	if len(body) > maxSubtitleBytes {
		return core.StreamInfo{}, fmt.Errorf("subtitle exceeds %d bytes", maxSubtitleBytes)
	}
	format = strings.TrimPrefix(strings.ToLower(format), ".")
	if filename != "" && format == "" {
		format = strings.ToLower(strings.TrimPrefix(path.Ext(filename), "."))
	}
	if !AcceptsUploadFormat(format) {
		return core.StreamInfo{}, fmt.Errorf("unsupported subtitle format %q", format)
	}
	lang := langtag.Normalize(language)
	if lang == "" {
		lang = languageFromName(filename)
	}
	if lang == "" {
		lang = "en"
	}
	label := strings.TrimSpace(filename)
	if ext := path.Ext(label); ext != "" {
		label = strings.TrimSuffix(label, ext)
	}
	if label == "" {
		label = strings.ToUpper(lang)
	}
	if len(label) > maxUploadLabel {
		label = strings.TrimSpace(label[:maxUploadLabel])
	}
	srt, vtt, err := NormalizeLang(body, lang)
	if err != nil {
		return core.StreamInfo{}, err
	}
	sum := sha1.Sum(body)
	fileID := int64(binary.BigEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
	if fileID == 0 {
		fileID = 1
	}
	id := cacheID(title, lang, fileID)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return core.StreamInfo{}, err
	}
	meta, _ := json.Marshal(map[string]any{
		"titleKey": title.Key.String(), "fileId": fileID, "language": lang,
		"release": uploadReleasePrefix + label, "fetchedAt": s.now().UTC(), "uploaded": true,
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
	sel := selection{ID: id, FileID: fileID, Language: lang, Release: uploadReleasePrefix + label, At: s.now().UTC()}
	p := s.uploadPath(title.Key, lang)
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

func languageFromName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "_", ".")
	name = strings.TrimSuffix(name, path.Ext(name))
	parts := strings.Split(name, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		if n := langtag.Normalize(parts[i]); n != "" {
			return n
		}
	}
	return ""
}

package transcode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/subtitles"
)

// One minute windows are addressed by exact release, source size, duration,
// track and codec. They survive HLS session retirement and process restart.
// Only cues observed by the active playback FFmpeg are written here.

type textSubtitleRead struct {
	offset  int64
	pending []byte
	format  string
}

func (s *Session) textSubtitleDir(index int) string {
	track := findStream(s.info, index, "subtitle")
	if s.textSubtitleSource == "" || track == nil {
		return ""
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("v1\x00%s\x00%d\x00%d\x00%d\x00%s", s.textSubtitleSource, s.info.Size, s.info.Duration, index, track.Codec)))
	return filepath.Join(s.m.tempDir, "subtitle-cues", hex.EncodeToString(key[:]))
}

func (s *Session) collectTextSubtitles(r *run) {
	if len(r.spec.TextSubtitlePaths) == 0 {
		return
	}
	if r.textSubtitles == nil {
		r.textSubtitles = make(map[int]*textSubtitleRead)
	}
	for index, path := range r.spec.TextSubtitlePaths {
		state := r.textSubtitles[index]
		if state == nil {
			state = &textSubtitleRead{}
			r.textSubtitles[index] = state
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.NewSectionReader(file, state.offset, 1<<20))
		file.Close()
		if err != nil || len(data) == 0 {
			continue
		}
		state.offset += int64(len(data))
		state.pending = append(state.pending, data...)
		var cues []subtitles.Cue
		for {
			i := bytes.IndexByte(state.pending, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimSpace(string(state.pending[:i]))
			state.pending = state.pending[i+1:]
			if strings.HasPrefix(line, "Format:") {
				state.format = line
			}
			if strings.HasPrefix(line, "Dialogue:") && state.format != "" {
				got, e := subtitles.ParseASS("[Events]\n" + state.format + "\n" + line + "\n")
				if e == nil {
					cues = append(cues, got...)
				}
			}
		}
		if len(state.pending) > 1<<20 {
			state.pending = nil
		}
		if len(cues) == 0 {
			continue
		}
		s.mu.Lock()
		dir := s.textSubtitleDir(index)
		s.mu.Unlock()
		if dir == "" {
			continue
		}
		windows := make(map[int64][]subtitles.Cue)
		for _, cue := range cues {
			for minute := int64(cue.Start / time.Minute); minute <= int64(cue.End/time.Minute) && minute <= int64(cue.Start/time.Minute)+10; minute++ {
				windows[minute] = append(windows[minute], cue)
			}
		}
		for minute, cues := range windows {
			if err := s.m.persistTextCues(dir, minute, cues); err != nil {
				s.log.Warn("persist playback subtitle cues", "error", err)
			}
		}
	}
}

func (m *Manager) persistTextCues(dir string, minute int64, cues []subtitles.Cue) error {
	m.textCueMu.Lock()
	defer m.textCueMu.Unlock()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("%08d.json", minute))
	existing := readTextCues(path)
	seen := make(map[subtitles.Cue]bool, len(existing))
	for _, cue := range existing {
		seen[cue] = true
	}
	for _, cue := range cues {
		if !seen[cue] {
			existing = append(existing, cue)
			seen[cue] = true
		}
	}
	raw, err := json.Marshal(existing)
	if err != nil {
		return err
	}
	if len(raw) > 8<<20 {
		return fmt.Errorf("subtitle window too large")
	}
	temp, err := os.CreateTemp(dir, ".window-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(raw)
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), path)
}
func readTextCues(path string) []subtitles.Cue {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var cues []subtitles.Cue
	if json.NewDecoder(io.LimitReader(file, 8<<20)).Decode(&cues) != nil {
		return nil
	}
	return cues
}

// TextSubtitleCues reads a bounded window from local disk, never the source.
func (s *Session) TextSubtitleCues(index int, from, to time.Duration) ([]subtitles.Cue, error) {
	s.mu.Lock()
	dir := s.textSubtitleDir(index)
	enabled := false
	for _, i := range s.textSubtitleStreams {
		if i == index {
			enabled = true
		}
	}
	s.mu.Unlock()
	if !enabled || dir == "" || from < 0 || to < from || to-from > 3*time.Minute {
		return nil, fmt.Errorf("subtitle track unavailable")
	}
	seen := make(map[subtitles.Cue]bool)
	var cues []subtitles.Cue
	for minute := int64(from / time.Minute); minute <= int64(to/time.Minute); minute++ {
		for _, cue := range readTextCues(filepath.Join(dir, fmt.Sprintf("%08d.json", minute))) {
			if cue.End >= from && cue.Start <= to && !seen[cue] {
				cues = append(cues, cue)
				seen[cue] = true
			}
		}
	}
	return cues, nil
}

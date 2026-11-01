package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// ErrSourceChanged signals an absent or different source revision.
var ErrSourceChanged = errors.New("playback: source revision changed")

// LocalFileSource exposes the opened file for change detection. The path and
// inode must still identify the same regular file when a revision is checked.
type LocalFileSource interface{ LocalFile() *os.File }

// localFileIdentity is change detection, not a content hash or a claim that
// a file cannot be modified. Missing platform file IDs fail closed.
func localFileIdentity(f *os.File) (any, bool) {
	if f == nil {
		return nil, false
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		return nil, false
	}
	current, err := os.Stat(f.Name())
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) || opened.Size() != current.Size() || !opened.ModTime().Equal(current.ModTime()) {
		return nil, false
	}
	dev, ino, ok := platformFileIdentity(f, opened)
	if !ok {
		return nil, false
	}
	return struct {
		Device, Inode uint64
		Size, MtimeNS int64
	}{dev, ino, opened.Size(), opened.ModTime().UnixNano()}, true
}

// SourceRevision identifies a release/file without exposing its name or
// upstream identifiers. Local revisions detect filesystem changes; they do
// not claim content immutability. Unsupported or unknown sources are empty.
func SourceRevision(s *PlaySession) string {
	if s == nil || s.Source == nil {
		return ""
	}
	id := s.Source.ReleaseID()
	if id == "" {
		return ""
	}
	if local, ok := s.Source.(LocalFileSource); ok {
		identity, valid := localFileIdentity(local.LocalFile())
		if !valid {
			return ""
		}
		data, _ := json.Marshal(struct {
			Key  RatingKey
			ID   string
			File any
		}{s.Key, id, identity})
		sum := sha256.Sum256(data)
		return "src-" + hex.EncodeToString(sum[:16])
	}
	if strings.HasPrefix(id, "local:") {
		return ""
	}
	name, size := s.Source.Name(), s.Source.Size()
	if name == "" || size <= 0 {
		return ""
	}
	data, _ := json.Marshal(struct {
		Key      RatingKey
		ID, Name string
		Size     int64
	}{s.Key, id, name, size})
	sum := sha256.Sum256(data)
	return "src-" + hex.EncodeToString(sum[:16])
}

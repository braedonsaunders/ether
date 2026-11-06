package plex

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/go-chi/chi/v5"
)

// The Get Info tree has a separate schema from normal metadata. Keep its
// unmeasured file details absent: Ether has no local file hashes to report.
type metadataTree struct {
	Size         int                `plex:"size"`
	MetadataItem []metadataTreeItem `plex:"MetadataItem"`
}
type metadataTreeItem struct {
	ParentTitle  string              `plex:"parentTitle"`
	ID           int64               `plex:"id"`
	Title        string              `plex:"title"`
	MetadataItem []metadataTreeItem  `plex:"MetadataItem"`
	MediaItem    []metadataTreeMedia `plex:"MediaItem"`
}
type metadataTreeMedia struct {
	ID        int64              `plex:"id"`
	MediaPart []metadataTreePart `plex:"MediaPart"`
}
type metadataTreePart struct {
	MetadataType *int                 `plex:"metadataType"`
	ID           int64                `plex:"id"`
	MediaStream  []metadataTreeStream `plex:"MediaStream"`
}
type metadataTreeStream struct {
	ID       int64        `plex:"id"`
	Index    *int         `plex:"index"`
	Type     int          `plex:"type"`
	Channels *int         `plex:"channels"`
	Extra    []model.Attr `plex:",attrs"`
}

func treeStream(st model.Stream) metadataTreeStream {
	out := metadataTreeStream{ID: st.ID, Index: st.Index, Type: st.StreamType}
	add := func(name, value string) {
		if value != "" {
			out.Extra = append(out.Extra, model.Attr{Name: name, Value: value})
		}
	}
	number := func(name string, value int) {
		if value != 0 {
			add(name, strconv.Itoa(value))
		}
	}
	flag := func(name string, value *bool) {
		if value != nil {
			if *value {
				add(name, "1")
			} else {
				add(name, "0")
			}
		}
	}
	add("codec", st.Codec)
	add("language", st.Language)
	add("languageCode", st.LanguageCode)
	add("title", st.Title)
	add("displayTitle", st.DisplayTitle)
	add("extendedDisplayTitle", st.ExtendedDisplayTitle)
	number("bitrate", st.Bitrate)
	number("bitDepth", st.BitDepth)
	number("width", st.Width)
	number("height", st.Height)
	add("profile", st.Profile)
	add("channelLayout", st.AudioChannelLayout)
	if st.StreamType == 2 {
		out.Channels = model.Ptr(st.Channels)
	}
	flag("default", st.Default)
	flag("forced", st.Forced)
	flag("hearingImpaired", st.HearingImpaired)
	flag("visualImpaired", st.VisualImpaired)
	return out
}

func (s *Server) handleMetadataTree(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	key, err := core.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	title, err := s.metadataTitle(r.Context(), pr, key)
	if err != nil || title == nil || !s.titleVisible(pr.User, title) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	node := metadataTreeItem{ID: int64(key), Title: title.Title}
	if title.Type == core.TypeSeason {
		node.Title = ""
		node.ParentTitle = title.ShowTitle
	}
	if isPlayableKey(key) {
		media, _ := s.mediaFor(r.Context(), pr, title, true, false, false)
		for _, m := range media {
			tm := metadataTreeMedia{ID: m.ID}
			for _, p := range m.Part {
				tp := metadataTreePart{ID: p.ID}
				if title.Type == core.TypeEpisode {
					tp.MetadataType = model.Ptr(4)
				}
				for _, st := range p.Stream {
					tp.MediaStream = append(tp.MediaStream, treeStream(st))
				}
				tm.MediaPart = append(tm.MediaPart, tp)
			}
			node.MediaItem = append(node.MediaItem, tm)
		}
	}
	// Episode reads describe exactly their ancestor chain, never siblings.
	if title.Type == core.TypeEpisode {
		season := metadataTreeItem{ID: int64(core.SeasonKey(key.TMDB(), key.Season())), MetadataItem: []metadataTreeItem{node}}
		node = metadataTreeItem{ID: int64(core.ShowKey(key.TMDB())), Title: title.ShowTitle, MetadataItem: []metadataTreeItem{season}}
	}
	s.write(w, r, metadataTree{Size: 1, MetadataItem: []metadataTreeItem{node}})
}

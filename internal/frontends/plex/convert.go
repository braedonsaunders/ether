package plex

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// renderOpts controls how much of an item is rendered.
type renderOpts struct {
	membershipChecked bool // the page was filtered before slicing
	detail            bool // full tags, Guid children, streams
	batchDetail       bool // multi-key metadata batch: keep list season counts for ungrouped shows
	slotPage          *slotMediaPage
	listRender        bool // multi-item render: never trigger plugin /resolve per row
	media             bool // Media/Part for playable items
	// inSection: the container already carries the section attributes
	// (section listings, children), so items omit librarySection*.
	inSection bool
	// detailEpisodes renders episodes in full (hub rows, like PMS).
	detailEpisodes bool
	// guids adds the external Guid children (related hubs, like PMS).
	guids bool
	// searchIdentity gives the leading search results a chance to carry the
	// canonical Plex GUID in the same response.
	searchIdentity bool
	// state holds the user's watch status for the rendered keys.
	state map[frontend.RatingKey]frontend.WatchStatus
	// skipLeafCounts skips applyListLeafCounts when the caller calls applyLeafCounts itself.
	skipLeafCounts bool
	// session renders /status/sessions: the Media is the one the session
	// was admitted on.
	session bool
	// storedOnly renders from stored catalogue rows only without scheduling
	// background TMDB detail, show-children, or Plex GUID refreshes.
	storedOnly bool
	// availabilityInSQL is set for a catalogue page that already applied
	// the same availability snapshot before counting and pagination.
	availabilityInSQL bool
	// mediaExcluded: the client excluded Media, so list rows skip building
	// it (and every read behind it). Set by renderTitles.
	mediaExcluded bool
	// skipExcludedMedia allows a detail batch that asked for nothing
	// needing Media (chapters, markers, streams, or file checks) to take
	// the same excludeElements=Media shortcut as list renders.
	skipExcludedMedia bool
	// vis overrides canonical section visibility when rendering rows scoped
	// to a specific library section (such as a custom library).
	vis *frontend.Visibility
	// section overrides canonical librarySection* item attributes when
	// rendering rows scoped to a specific library section.
	section *Section
}

// metadataKey is the item's "key": children list for containers, the item
// itself for playables.
func metadataKey(k frontend.RatingKey, t frontend.MediaType) string {
	switch t {
	case frontend.TypeShow, frontend.TypeSeason:
		return "/library/metadata/" + k.String() + "/children"
	}
	return "/library/metadata/" + k.String()
}

func isPlayable(t frontend.MediaType) bool {
	return t == frontend.TypeMovie || t == frontend.TypeEpisode || t == "track"
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

// guidFor returns the primary guid attribute. Ether has no plex:// guids, so
// it uses the legacy TMDB agent form, which every client understands as a
// legacy-agent item.
func guidFor(t *frontend.Title) string {
	switch t.Key.Kind() {
	case frontend.KindLocal:
		h := fnv.New64a()
		fmt.Fprintf(h, "%d", int64(t.Key))
		return fmt.Sprintf("com.plexapp.agents.none://%016x?lang=xn", h.Sum64())
	}
	switch t.Type {
	case frontend.TypeMovie, frontend.TypeShow:
		if t.IDs.TMDB > 0 {
			return tmdbAgentGUID(strconv.FormatInt(t.IDs.TMDB, 10))
		}
	case frontend.TypeSeason:
		if t.ShowIDs.TMDB > 0 && !t.Key.Allocated() {
			return tmdbAgentGUID(fmt.Sprintf("%d/%d", t.ShowIDs.TMDB, t.Key.Season()))
		}
	case frontend.TypeEpisode:
		if t.ShowIDs.TMDB > 0 && !t.Key.Allocated() {
			return tmdbAgentGUID(fmt.Sprintf("%d/%d/%d", t.ShowIDs.TMDB, t.Key.Season(), t.Key.Episode()))
		}
	}
	if t.IDs.TVDB > 0 {
		return "tvdb://" + strconv.FormatInt(t.IDs.TVDB, 10)
	}
	if t.IDs.IMDB != "" {
		return "imdb://" + t.IDs.IMDB
	}
	return localAgentGUID(t.Key)
}

func tmdbAgentGUID(id string) string { return "com.plexapp.agents.themoviedb://" + id + "?lang=en" }
func localAgentGUID(key frontend.RatingKey) string {
	return "com.plexapp.agents.none://" + key.String() + "?lang=xn"
}

func showGUID(show frontend.RatingKey, ids frontend.IDs) string {
	return guidFor(&frontend.Title{Key: show, Type: frontend.TypeShow, IDs: ids})
}

// titleParent and titleShow prefer projected relations. Canonical packed keys
// still encode their catalogue hierarchy on rows without a projected parent.
func titleParent(t *frontend.Title) frontend.RatingKey {
	if t.ParentKey != 0 {
		return t.ParentKey
	}
	if t.Type == frontend.TypeSeason && t.ShowKey != 0 {
		return t.ShowKey
	}
	return t.Key.Parent()
}

func titleShow(t *frontend.Title) frontend.RatingKey {
	if t.Type == frontend.TypeShow {
		return t.Key
	}
	if t.ShowKey != 0 {
		return t.ShowKey
	}
	parent := titleParent(t)
	if t.Type == frontend.TypeEpisode {
		return parent.Parent()
	}
	if t.Type == frontend.TypeSeason {
		return parent
	}
	return 0
}

// externalGuids are the <Guid id="..."/> children.
func externalGuids(t *frontend.Title) []model.GUID {
	var out []model.GUID
	if t.IDs.IMDB != "" {
		out = append(out, model.GUID{ID: "imdb://" + t.IDs.IMDB})
	}
	if t.IDs.TMDB > 0 {
		out = append(out, model.GUID{ID: "tmdb://" + strconv.FormatInt(t.IDs.TMDB, 10)})
	}
	if t.IDs.TVDB > 0 {
		out = append(out, model.GUID{ID: "tvdb://" + strconv.FormatInt(t.IDs.TVDB, 10)})
	}
	return out
}

func tagID(kind, name string) int64 {
	h := fnv.New32a()
	h.Write([]byte(kind + ":" + strings.ToLower(name)))
	return int64(h.Sum32()&0x7fffffff) + 1
}

func imageStamp(t *frontend.Title) string {
	if ts := unixOrZero(t.UpdatedAt); ts > 0 {
		return strconv.FormatInt(ts, 10)
	}
	return "1"
}

func artIdentityStamp(u string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(u))
	return strconv.FormatUint(uint64(h.Sum32()), 10)
}

func overlayThumbStamp(t *frontend.Title, row frontend.CachedMetadataOverlay, ok bool) string {
	if ok && row.ThumbURL != "" && (t.PosterPath == frontend.RemoteMetadataArt || t.StillPath == frontend.RemoteMetadataArt) {
		return artIdentityStamp(row.ThumbURL)
	}
	return imageStamp(t)
}

func overlayArtStamp(t *frontend.Title, row frontend.CachedMetadataOverlay, ok bool) string {
	if ok && row.ArtURL != "" && t.BackdropPath == frontend.RemoteMetadataArt {
		return artIdentityStamp(row.ArtURL)
	}
	return imageStamp(t)
}

func (s *Server) posterGeneration() uint64 {
	if s == nil {
		return 0
	}
	if d := s.discovery(); d != nil && d.Overlays != nil {
		if pg, ok := d.Overlays.(frontend.PosterGenerationSource); ok {
			if gen := pg.PosterGeneration(); gen > 0 {
				return gen
			}
		}
	}
	if art := s.getHost().Artwork; art != nil {
		if pg, ok := art.(frontend.PosterGenerationSource); ok {
			return pg.PosterGeneration()
		}
	}
	return 0
}

func (s *Server) posterStamp(stamp string) string {
	if gen := s.posterGeneration(); gen > 0 {
		return stamp + "-p" + strconv.FormatUint(gen, 10)
	}
	return stamp
}

// thumbURL / artURL are our own artwork endpoints for an item.
func (s *Server) thumbURL(t *frontend.Title) string {
	return s.stampedThumbURL(t, imageStamp(t))
}

func (s *Server) stampedThumbURL(t *frontend.Title, stamp string) string {
	poster := t.Type == frontend.TypeMovie || t.Type == frontend.TypeShow ||
		(t.Type == "" && (t.Key.Kind() == frontend.KindMovie || t.Key.Kind() == frontend.KindShow))
	if version := s.ownerArtworkVersion(t, frontend.ImagePoster); version != "" {
		if poster {
			version = s.posterStamp(version)
		}
		return "/library/metadata/" + t.Key.String() + "/thumb/" + version
	}
	if t.PosterPath == "" && t.StillPath == "" {
		// A movie or show with no artwork gets Ether's generated poster.
		// Its stamp names the drawn image, so the URL changes (and client
		// caches refetch) when the title gains a real poster.
		if s != nil {
			if st, ok := frontend.TitlePosterStamp(t); ok {
				if poster {
					st = s.posterStamp(st)
				}
				return "/library/metadata/" + t.Key.String() + "/thumb/" + st
			}
		}
		return ""
	}
	if poster {
		stamp = s.posterStamp(stamp)
	}
	return "/library/metadata/" + t.Key.String() + "/thumb/" + stamp
}

func (s *Server) artURL(t *frontend.Title) string {
	return s.stampedArtURL(t, imageStamp(t))
}

func (s *Server) stampedArtURL(t *frontend.Title, stamp string) string {
	if version := s.ownerArtworkVersion(t, frontend.ImageBackdrop); version != "" {
		return "/library/metadata/" + t.Key.String() + "/art/" + s.posterStamp(version)
	}
	if t.BackdropPath == "" {
		return ""
	}
	return "/library/metadata/" + t.Key.String() + "/art/" + stamp
}

func seasonTitle(t *frontend.Title) string {
	if t.SeasonTitle != "" {
		return t.SeasonTitle
	}
	if t.Season == 0 {
		return "Specials"
	}
	return "Season " + strconv.Itoa(t.Season)
}

// metadata renders one title.
func (s *Server) metadata(ctx context.Context, pr *Request, t *frontend.Title, o renderOpts) model.Metadata {
	var m model.Metadata
	s.metadataInto(ctx, pr, t, o, &m)
	return m
}

func (s *Server) metadataInto(ctx context.Context, pr *Request, t *frontend.Title, o renderOpts, m *model.Metadata) {
	if !o.storedOnly {
		t = s.enrich(t)
	}
	added := t.DateAdded(s.now())
	detail := o.detail || (o.detailEpisodes && t.Type == frontend.TypeEpisode)
	ov, hasPlex := s.plexMetaOverlay(ctx, t.Key)
	var (
		credits    frontend.LibraryTitleCredits
		hasCredits bool
		creditsPtr *frontend.LibraryTitleCredits
	)
	if detail {
		credits, hasCredits = s.plexCreditsCached(ctx, t.Key)
		if hasCredits {
			creditsPtr = &credits
		}
	}
	tmdbRating := t.Rating
	if hasPlex {
		t = applyPlexMetaTitle(t, ov.Overlay)
	}
	thumbStamp := overlayThumbStamp(t, ov, hasPlex)
	artStamp := overlayArtStamp(t, ov, hasPlex)
	show := s.showOfOpts(t, o.storedOnly)
	*m = model.Metadata{
		RatingKey:     t.Key.String(),
		Key:           metadataKey(t.Key, t.Type),
		GUID:          guidFor(t),
		Type:          s.plexTypeFor(t),
		Title:         t.Title,
		Summary:       t.Summary,
		Tagline:       t.Tagline,
		Year:          t.Year,
		ContentRating: t.ContentRating,
		Thumb:         s.stampedThumbURL(t, thumbStamp),
		Art:           s.stampedArtURL(t, artStamp),
		AddedAt:       unixOrZero(added),
		UpdatedAt:     unixOrZero(t.UpdatedAt),
	}
	if t.SortTitle != "" && t.SortTitle != t.Title {
		m.TitleSort = t.SortTitle
	}
	if t.OriginalTitle != "" && t.OriginalTitle != t.Title {
		m.OriginalTitle = t.OriginalTitle
	}
	if !t.Released.IsZero() {
		m.OriginallyAvailableAt = cmp.Or(t.EpisodeAirDate, t.Released).Format("2006-01-02")
		if m.Year == 0 {
			m.Year = t.Released.Year()
		}
	}
	if t.Runtime > 0 {
		m.Duration = ms(t.Runtime)
	}
	if hasPlex && ov.AudienceRating > 0 {
		m.AudienceRating = math.Round(ov.AudienceRating*10) / 10
		m.AudienceRatingImage = ov.AudienceRatingImage
	} else if tmdbRating > 0 {
		m.AudienceRating = math.Round(tmdbRating*10) / 10
		m.AudienceRatingImage = "themoviedb://image.rating"
	}
	if hasPlex && ov.Rating > 0 {
		m.Rating = ov.Rating
		m.RatingImage = ov.RatingImage
	}
	if len(t.Studios) > 0 {
		m.Studio = t.Studios[0]
	}
	var sec Section
	var secOK bool
	if o.section != nil && o.section.ID != 0 {
		sec, secOK = *o.section, true
	} else if !o.inSection && t.Key.Kind() != frontend.KindCollection {
		sec, secOK = s.sectionForTitle(ctx, t)
	}
	if secOK && !o.inSection && t.Key.Kind() != frontend.KindCollection {
		m.LibrarySectionID = sec.ID
		m.LibrarySectionKey = "/library/sections/" + strconv.Itoa(sec.ID)
		m.LibrarySectionTitle = sec.Title
	}

	if m.Type == "playlist" {
		m.Key = "/playlists/" + m.RatingKey + "/items"
		m.PlaylistType = "audio"
		m.Smart = model.Ptr(false)
		m.LeafCount = model.Ptr(t.ChildCount)
		m.Composite = m.Thumb
		return
	}

	switch t.Type {
	case frontend.TypeMovie:
		m.Slug = ov.Slug
		if m.Slug == "" {
			m.Slug = credits.Slug
		}
	case frontend.TypeShow:
		m.Slug = ov.Slug
		if m.Slug == "" {
			m.Slug = credits.Slug
		}
		m.Index = model.Ptr(1)
		if t.ChildCount > 0 {
			m.ChildCount = model.Ptr(t.ChildCount)
		}
		m.LeafCount = model.Ptr(t.LeafCount)
		m.ViewedLeafCount = model.Ptr(0)
	case frontend.TypeSeason:
		// PMS seasons carry the show's slug/studio/year as parent*, and no
		// dates, ratings, sort title or genre/country tags of their own.
		m.Index = model.Ptr(t.Season)
		if t.Title == "" {
			m.Title = seasonTitle(t)
		}
		showKey := titleShow(t)
		if showKey != 0 {
			m.ParentRatingKey = showKey.String()
			m.ParentGUID = showGUID(showKey, t.ShowIDs)
			m.ParentKey = "/library/metadata/" + showKey.String()
			m.ParentThumb = "/library/metadata/" + showKey.String() + "/thumb/" + s.posterStamp(imageStamp(t))
			if m.Art == "" {
				m.Art = "/library/metadata/" + showKey.String() + "/art/" + imageStamp(t)
			}
			if m.Thumb == "" {
				m.Thumb = m.ParentThumb
			}
		}
		m.ParentTitle = t.ShowTitle
		if m.ParentTitle == "" {
			m.ParentTitle = t.ParentTitle
		}
		if showKey != 0 && !showKey.IsPluginKey() {
			m.ParentSlug = s.verifiedPlexSlug(ctx, showKey, detail)
		}
		m.ParentIndex = model.Ptr(1)
		if show != nil {
			if len(show.Studios) > 0 {
				m.ParentStudio = show.Studios[0]
			}
			m.ParentYear = show.Year
			if m.ParentTitle == "" {
				m.ParentTitle = show.Title
			}
		}
		if m.ParentYear == 0 && t.ShowYear > 0 {
			m.ParentYear = t.ShowYear
		}
		leafCount := t.LeafCount
		if leafCount == 0 {
			leafCount = t.ChildCount
		}
		m.LeafCount = model.Ptr(leafCount)
		m.ViewedLeafCount = model.Ptr(0)
		if !t.Key.IsPluginKey() {
			m.Year, m.OriginallyAvailableAt, m.TitleSort = 0, "", ""
			m.ContentRating, m.AudienceRating, m.AudienceRatingImage = "", 0, ""
			m.Studio = ""
		}
	case frontend.TypeEpisode:
		// PMS episodes have no year, sort title or genre/country tags; the
		// show's slug and content rating are carried over.
		m.Index = model.Ptr(t.Episode)
		parentIdx := t.Season
		if t.Key.IsPluginKey() && parentIdx == 0 && t.Disc > 0 {
			parentIdx = t.Disc
		}
		m.ParentIndex = model.Ptr(parentIdx)
		season := titleParent(t)
		showKey := titleShow(t)
		if season != 0 {
			m.ParentRatingKey = season.String()
			m.ParentGUID = localAgentGUID(season)
			if t.ShowIDs.TMDB > 0 && !season.Allocated() && !season.IsPluginKey() {
				m.ParentGUID = tmdbAgentGUID(fmt.Sprintf("%d/%d", t.ShowIDs.TMDB, season.Season()))
			}
			m.ParentKey = "/library/metadata/" + season.String()
			m.ParentThumb = "/library/metadata/" + season.String() + "/thumb/" + imageStamp(t)
			if t.Key.IsPluginKey() && m.Thumb == "" {
				m.Thumb = m.ParentThumb
			}
		}
		if t.Key.IsPluginKey() {
			m.ParentTitle = t.ParentTitle
			if m.ParentTitle == "" {
				m.ParentTitle = t.SeasonTitle
			}
		} else {
			m.ParentTitle = seasonTitle(t)
		}
		if showKey != 0 {
			m.GrandparentRatingKey = showKey.String()
			if t.Key.IsPluginKey() {
				m.GrandparentGUID = "com.plexapp.agents.none://" + showKey.String() + "?lang=xn"
			} else {
				m.GrandparentGUID = showGUID(showKey, t.ShowIDs)
			}
			m.GrandparentKey = "/library/metadata/" + showKey.String()
			m.GrandparentTitle = t.ShowTitle
			m.GrandparentThumb = "/library/metadata/" + showKey.String() + "/thumb/" + s.posterStamp(imageStamp(t))
			m.GrandparentArt = "/library/metadata/" + showKey.String() + "/art/" + imageStamp(t)
			if m.Art == "" {
				m.Art = m.GrandparentArt
			}
		}
		if show != nil {
			if m.GrandparentTitle == "" {
				m.GrandparentTitle = show.Title
			}
			if m.ContentRating == "" {
				m.ContentRating = show.ContentRating
			}
		}
		if showKey != 0 && !showKey.IsPluginKey() {
			m.GrandparentSlug = s.verifiedPlexSlug(ctx, showKey, detail)
		}
		if t.Key.IsPluginKey() {
			m.TitleSort, m.Studio = "", ""
		} else {
			m.Year, m.TitleSort, m.Studio = 0, "", ""
		}
	}
	if hasPlex {
		applyPlexMetaDates(t, m, ov.Overlay)
		if ov.Rating > 0 && t.Type != frontend.TypeSeason && t.Type != frontend.TypeEpisode {
			m.Rating = ov.Rating
			m.RatingImage = ov.RatingImage
		}
	}

	// Per-user state.
	if st, ok := o.state[t.Key]; ok {
		m.ViewCount = st.ViewCount
		if st.ViewOffset > 0 && isPlayable(t.Type) {
			m.ViewOffset = ms(st.ViewOffset)
		} else if st.Reading != nil && st.Reading.Fraction > 0 && t.Runtime > 0 && isPlayable(t.Type) {
			m.ViewOffset = ms(time.Duration(st.Reading.Fraction * float64(t.Runtime)))
		}
		m.LastViewedAt = unixOrZero(st.LastViewedAt)
		m.LastRatedAt = unixOrZero(st.LastRatedAt)
		if st.UserRating > 0 {
			m.UserRating = st.UserRating
			m.LastRatedAt = unixOrZero(st.LastRatedAt)
		}
	}

	// Tags. Detail episodes inside hubs retain full tags. Light rows cap
	// genre/country/writer at two and director/cast at three.
	maxTags := 2
	maxPeople := 3
	if detail {
		maxTags = math.MaxInt
		maxPeople = math.MaxInt
	}
	genres, countries := t.Genres, t.Countries
	if t.Type == frontend.TypeSeason || t.Type == frontend.TypeEpisode {
		genres, countries = nil, nil
	}
	for i, g := range genres {
		if i >= maxTags {
			break
		}
		tag := model.Tag{Tag: g}
		if detail {
			tag.ID = tagID("genre", g)
			tag.Filter = "genre=" + url.QueryEscape(g)
		}
		m.Genre = append(m.Genre, tag)
	}
	for i, c := range countries {
		if i >= maxTags {
			break
		}
		tag := model.Tag{Tag: c}
		if detail {
			tag.ID = tagID("country", c)
			tag.Filter = "country=" + url.QueryEscape(c)
		}
		m.Country = append(m.Country, tag)
	}
	m.Director = s.personTags(t.Key, t.Directors, "director", maxPeople, detail, false, creditsPtr, nil)
	m.Writer = s.personTags(t.Key, t.Writers, "writer", maxTags, detail, false, creditsPtr, nil)
	m.Role = s.personTags(t.Key, t.Cast, "actor", maxPeople, detail, true, creditsPtr, nil)
	if detail && t.Type != frontend.TypeSeason {
		m.Producer = s.personTags(t.Key, detailProducers(t, creditsPtr), "producer", maxTags, detail, false, creditsPtr, nil)
	}
	if t.Type == frontend.TypeMovie || t.Type == frontend.TypeShow {
		if hasCredits && credits.ContentRatingAge > 0 {
			m.ContentRatingAge = credits.ContentRatingAge
		} else if age := contentRatingAge(m.ContentRating); age > 0 {
			m.ContentRatingAge = age
		}
	}
	if t.CollectionID > 0 && t.CollectionName != "" {
		tag := model.Tag{Tag: t.CollectionName}
		if detail {
			tag.ID = t.CollectionID
			tag.Filter = "collection=" + strconv.FormatInt(t.CollectionID, 10)
		}
		m.Collection = []model.Tag{tag}
	}
	if detail {
		m.Guid = externalGuids(t)
	} else if o.guids {
		m.GuidLead = externalGuids(t)
	}
	if detail {
		if m.AudienceRating > 0 && m.AudienceRatingImage != "" {
			m.Rating_ = []model.RatingTag{{Image: m.AudienceRatingImage, Value: m.AudienceRating, Type: "audience"}}
		}
	}
	// PMS sends Image (and, for movies and shows, UltraBlurColors) on
	// every item, in listings and hubs as well as detail pages.
	m.Image = s.imageTags(t, show, thumbStamp, artStamp)
	if ultraBlurType(t.Type) {
		m.UltraBlurColors = s.ultraBlurFor(t.Key, t.BackdropPath)
	}

	withMedia := o.media
	if o.mediaExcluded && isPlayable(t.Type) {
		if t.Runtime > 0 {
			// What building Media would leave behind once it is excluded.
			if s.downloadsAllowed(pr.User) {
				m.AllowSync = model.Ptr(true)
			}
		} else {
			withMedia = true // the row's duration comes from its Media
		}
	}
	if withMedia && isPlayable(t.Type) {
		var editionTitle string
		var info frontend.MediaInfo
		var selectedChapterMedia frontend.MediaInfo
		withVariants := o.detail && !o.batchDetail
		if t.Key.IsPluginKey() && (o.listRender || o.batchDetail || o.detailEpisodes || libraryToolClient(pr)) {
			// Plugin /resolve must only run on single-item detail or play
			// intent, never per row in a list, hub, On Deck, or batch lookup.
			withVariants = false
		}
		finish := func(media []model.Media) {
			m.Media = media
			if m.Type == "track" && t.Key.IsPluginKey() {
				if hint, ok := s.exp().(frontend.TitleLyricsHint); ok {
					s.addTrackLyrics(ctx, pr.User, m.Media, t.Key, hint.TitleLyricsAvailability(*t))
				}
			}
			applyEditionTitle(m, editionTitle)
			if s.downloadsAllowed(pr.User) {
				m.AllowSync = model.Ptr(true)
			}
			if detail {
				s.addProbedStreamFeatures(ctx, t.Key, info, m)
				if len(m.Chapter) == 0 && len(selectedChapterMedia.Chapters) > 0 {
					addMediaChapters(t.Key, selectedChapterMedia, m)
				}
			}
			if m.Duration == 0 && len(m.Media) > 0 {
				m.Duration = m.Media[0].Duration
			}
		}
		var deferSlots func([]frontend.VariantRef, slotMediaRender)
		deferred := false
		if o.slotPage != nil {
			deferSlots = func(refs []frontend.VariantRef, render slotMediaRender) {
				deferred = true
				o.slotPage.refs = append(o.slotPage.refs, refs...)
				o.slotPage.finish = append(o.slotPage.finish, func(ids map[frontend.VariantRef]int64) {
					finish(render(ids))
				})
			}
		}
		var media []model.Media
		media, editionTitle, info = s.mediaForWithInfo(ctx, pr, t, detail, withVariants, o.session, o.batchDetail, deferSlots, &selectedChapterMedia)
		if !deferred {
			finish(media)
		}
	}
	s.decorateDiscoveryMetadata(ctx, pr, t, show, m, o.detail)
}

// imageTags are an item's <Image> children, composed like PMS's:
//
//	movie, show: coverPoster, background, clearLogo
//	season:      coverPoster (season, else show), background (show), clearLogo (show)
//	episode:     coverPoster (show), snapshot (still), background (show), clearLogo (show)
//
// Only artwork Ether can serve is listed; the show's clear logo needs the
// show's detail (show), which may not be cached on the first render.
func (s *Server) imageTags(t *frontend.Title, show *frontend.Title, thumbStamp, artStamp string) []model.Image {
	alt := t.Title
	if alt == "" && t.Type == frontend.TypeSeason {
		alt = seasonTitle(t)
	}
	img := func(typ, url string) model.Image { return model.Image{Alt: alt, Type: typ, URL: url} }
	if thumbStamp == "" {
		thumbStamp = imageStamp(t)
	}
	if artStamp == "" {
		artStamp = imageStamp(t)
	}
	var out []model.Image
	switch t.Type {
	case frontend.TypeSeason, frontend.TypeEpisode:
		showKey := t.ShowKey
		if showKey == 0 {
			showKey = t.Key.Parent()
			if t.Type == frontend.TypeEpisode {
				showKey = showKey.Parent()
			}
		}
		showPath := "/library/metadata/" + showKey.String()
		showThumbStamp := s.posterStamp(thumbStamp)
		if t.Type == frontend.TypeSeason {
			if th := s.stampedThumbURL(t, thumbStamp); th != "" {
				out = append(out, img("coverPoster", th))
			} else {
				out = append(out, img("coverPoster", showPath+"/thumb/"+showThumbStamp))
			}
		} else {
			out = append(out, img("coverPoster", showPath+"/thumb/"+showThumbStamp))
			if th := s.stampedThumbURL(t, thumbStamp); th != "" {
				out = append(out, img("snapshot", th))
			}
		}
		if a := s.stampedArtURL(t, artStamp); a != "" {
			out = append(out, img("background", a))
		} else {
			out = append(out, img("background", showPath+"/art/"+artStamp))
		}
		if show != nil && show.LogoPath != "" {
			out = append(out, img("clearLogo", showPath+"/clearLogo/"+imageStamp(t)))
		}
	default:
		if th := s.stampedThumbURL(t, thumbStamp); th != "" {
			out = append(out, img("coverPoster", th))
		}
		if a := s.stampedArtURL(t, artStamp); a != "" {
			out = append(out, img("background", a))
		}
		if t.LogoPath != "" {
			out = append(out, img("clearLogo", "/library/metadata/"+t.Key.String()+"/clearLogo/"+imageStamp(t)))
		}
	}
	return out
}

func (s *Server) personTags(key frontend.RatingKey, ps []frontend.Person, kind string, max int, detail, withRole bool, credits *frontend.LibraryTitleCredits, mapped *[]frontend.LibraryPersonMapping) []model.Tag {
	var out []model.Tag
	for i, p := range ps {
		if i >= max {
			break
		}
		tag := model.Tag{Tag: p.Name}
		if detail {
			if p.TMDB > 0 {
				tag.ID = p.TMDB
				tag.Filter = kind + "=" + strconv.FormatInt(p.TMDB, 10)
			} else {
				tag.ID = tagID(kind, p.Name)
			}
			var (
				matched  frontend.LibraryCredit
				hasMatch bool
			)
			if credits != nil {
				matched, hasMatch = credits.Match(kind, p)
			}
			// Emit tagKey ONLY when it is a real 24-hex plex://person id:
			// Plex Web with Discover Credits routes any non-empty tagKey to
			// discover.provider.plex.tv/library/people/<tagKey>, so a
			// fabricated tmdb-person-<id> errors (#653), while native Plex
			// apps require a real tagKey to render Cast & Crew.
			if hasMatch && frontend.IsLibraryPersonTagKey(matched.TagKey) {
				tag.TagKey = matched.TagKey
				if mapped != nil && p.TMDB > 0 {
					*mapped = append(*mapped, frontend.LibraryPersonMapping{
						TagKey:     matched.TagKey,
						PersonTMDB: p.TMDB,
						Name:       p.Name,
						Thumb:      matched.Thumb,
					})
				}
			}
			if withRole {
				tag.Role = p.Role
			}
			thumbPath := p.ThumbPath
			if thumbPath == "" && hasMatch && plexImageURL(matched.Thumb) {
				thumbPath = matched.Thumb
			}
			if thumbPath != "" {
				tag.Thumb = "/library/metadata/" + key.String() + "/person/" + kind + "/" + base64.RawURLEncoding.EncodeToString([]byte(thumbPath))
			}
		}
		out = append(out, tag)
	}
	return out
}

// mediaInfoFor is the media info shown for a title: the live session's when
// one exists for this client, otherwise the probe of the release Prepare
// would still open for this viewer, otherwise the probe a detail-page
// warm-up already completed for this client, otherwise the playback
// estimate. Clients build their track list from the detail/play-queue
// item they navigated with, so once a warm-up has probed the file its
// real tracks must be rendered there: the estimate invents a single
// audio track and no subtitles.
type resumedVersionResult struct {
	info     frontend.MediaInfo
	rel      frontend.Release
	selector string
	ok       bool
}

func (s *Server) mediaInfoWithResumed(ctx context.Context, pr *Request, t *frontend.Title) (frontend.MediaInfo, *frontend.PlaySession, resumedVersionResult, bool) {
	if s.playback() == nil || pr == nil {
		dur := time.Duration(0)
		if t != nil {
			dur = t.Runtime
		}
		return frontend.MediaInfo{Estimated: true, Duration: dur}, nil, resumedVersionResult{}, false
	}
	if pr.User != nil {
		if ps, ok := s.playback().SessionFor(pr.User.ID, pr.ClientID, t.Key); ok && ps != nil {
			return ps.Info, ps, resumedVersionResult{}, !ps.Info.Estimated && len(ps.Info.Streams) > 0
		}
		if info, rel, sel, ok := s.resumedVersion(ctx, pr, t); ok {
			return info, nil, resumedVersionResult{info: info, rel: rel, selector: sel, ok: true}, true
		}
		if wp, ok := s.playback().(frontend.WarmProber); ok {
			caps := s.clientCapsForKey(pr, t.Key)
			if info, warm := wp.WarmProbeInfo(pr.User, caps, t.Key); warm {
				return info, nil, resumedVersionResult{}, true
			}
		}
	}
	info, ps := frontend.Estimate(ctx, s.playback(), pr.User, pr.ClientID, t, func() frontend.ClientCaps {
		return s.clientCapsForKey(pr, t.Key)
	})
	return info, ps, resumedVersionResult{}, false
}

func (s *Server) mediaInfoWithSource(ctx context.Context, pr *Request, t *frontend.Title) (frontend.MediaInfo, *frontend.PlaySession, bool) {
	info, ps, _, probed := s.mediaInfoWithResumed(ctx, pr, t)
	return info, ps, probed
}

func (s *Server) mediaInfoFor(ctx context.Context, pr *Request, t *frontend.Title) (frontend.MediaInfo, *frontend.PlaySession) {
	info, ps, _ := s.mediaInfoWithSource(ctx, pr, t)
	return info, ps
}

// resumedTrackSource is implemented by the playback manager. It returns the
// probed streams of the release ResumeFor would still open: same viewer,
// same profile selection key, source still allowed, release not blocked,
// and the payload hash and size still match. An empty release ID is not
// a hit. There is no separate time-to-live; a profile, source, block, or
// release change drops the row the same way the next Prepare does.
type resumedTrackSource interface {
	ResumedTracks(context.Context, *frontend.User, frontend.RatingKey, frontend.ClientCaps) (frontend.MediaInfo, string, bool)
}

type resumedVersionSource interface {
	ResumedVersion(context.Context, *frontend.User, frontend.RatingKey, frontend.ClientCaps) (frontend.MediaInfo, frontend.Release, string, bool)
}

func (s *Server) resumedTracks(ctx context.Context, pr *Request, t *frontend.Title) (frontend.MediaInfo, string, bool) {
	info, rel, _, ok := s.resumedVersion(ctx, pr, t)
	if !ok {
		return frontend.MediaInfo{}, "", false
	}
	return info, rel.ID, true
}

func (s *Server) resumedVersion(ctx context.Context, pr *Request, t *frontend.Title) (frontend.MediaInfo, frontend.Release, string, bool) {
	if pr == nil || pr.User == nil || pr.User.ID <= 0 || t == nil || s.playback() == nil || !s.playableTitle(ctx, pr.User, t) {
		return frontend.MediaInfo{}, frontend.Release{}, "", false
	}
	caps := s.clientCapsForKey(pr, t.Key)
	if src, ok := s.playback().(resumedVersionSource); ok {
		info, rel, sel, ok := src.ResumedVersion(ctx, pr.User, t.Key, caps)
		if !ok || rel.ID == "" || info.Estimated || !info.HasPlayableMedia() {
			return frontend.MediaInfo{}, frontend.Release{}, "", false
		}
		return info, rel, sel, true
	}
	src, ok := s.playback().(resumedTrackSource)
	if !ok {
		return frontend.MediaInfo{}, frontend.Release{}, "", false
	}
	info, releaseID, ok := src.ResumedTracks(ctx, pr.User, t.Key, caps)
	if !ok || releaseID == "" || info.Estimated || !info.HasPlayableMedia() {
		return frontend.MediaInfo{}, frontend.Release{}, "", false
	}
	return info, frontend.Release{ID: releaseID}, "", true
}

// mediaFor renders the configured menu on every playable Media surface.
// Providers without configured slots expose their actual editions on detail
// pages. The second result is editionTitle; session monitoring projects only
// the admitted source. withVariants permits cached labels on detail/queue.
func (s *Server) mediaFor(ctx context.Context, pr *Request, t *frontend.Title, withStreams, withVariants, sessionMedia bool) ([]model.Media, string) {
	media, edition, _ := s.mediaForWithInfo(ctx, pr, t, withStreams, withVariants, sessionMedia, false, nil, nil)
	return media, edition
}

func (s *Server) mediaForWithInfo(ctx context.Context, pr *Request, t *frontend.Title, withStreams, withVariants, sessionMedia, batchDetail bool, deferSlots func([]frontend.VariantRef, slotMediaRender), selectedChapterMedia *frontend.MediaInfo) ([]model.Media, string, frontend.MediaInfo) {
	info, ps, resumed, probedSource := s.mediaInfoWithResumed(ctx, pr, t)
	singleDetail := !batchDetail && withStreams && pr != nil && pr.versionItemKey == t.Key
	if ps == nil && singleDetail && pr.User != nil {
		// Explicit decisions are variant-scoped. Match the release admitted
		// for this client instead of taking an arbitrary session for the title.
		pin := s.playIDs.pinned(s.playIdentityFor(pr, t.Key), false, s.now())
		if scoped, ok := s.playback().(frontend.SurfaceScopedPlayback); ok && pin.admittedRelease != "" {
			if active, ok := scoped.SessionForSurface("plex", pr.User.ID, pr.ClientID, t.Key, pin.admittedRelease, false); ok && active != nil {
				info, ps, resumed = active.Info, active, resumedVersionResult{}
				probedSource = !info.Estimated && len(info.Streams) > 0
			}
		}
	}
	if singleDetail && probedSource {
		pr.detailTracksKnown = true
	}
	stamp := unixOrZero(t.UpdatedAt)
	if ps != nil && !ps.StartedAt.IsZero() {
		stamp = ps.StartedAt.Unix()
	}
	if stamp <= 0 {
		stamp = 1
	}
	if source, ok := s.svc().variants.(frontend.VersionSlots); ok && !sessionMedia {
		caps := s.clientCapsForKey(pr, t.Key)
		// Menu ranking describes the source, just like admission. Remote and
		// requested bitrate ceilings belong to the output decision.
		caps.MaxBitrateKbps, caps.DefaultRemoteBitrate = 0, false
		req := frontend.ResolveRequest{Title: t, Client: caps, User: pr.User}
		if ps != nil && ps.Resolution != nil {
			req.PlayedRelease = ps.Resolution.Release.ID
			req.PlayedSelector = ps.Resolution.VersionSelector
			relCopy := ps.Resolution.Release
			req.PlayedReleaseRecord = &relCopy
		} else if resumed.ok {
			req.PlayedRelease = resumed.rel.ID
			req.PlayedSelector = resumed.selector
			relCopy := resumed.rel
			req.PlayedReleaseRecord = &relCopy
		}
		vctx := ctx
		if pr != nil && pr.playIntentVersionKey == t.Key {
			vctx = frontend.WithoutLocalResponse(vctx)
		}
		versions, err := source.VersionSlots(vctx, req, frontend.MaxVariants, withVariants && withStreams)
		if err != nil {
			pr.versionErr = err
			return nil, "", info
		}
		if len(versions) > 0 {
			pr.configuredSlots = true
			if singleDetail && versions[0].Auto && !versions[0].Info.Estimated && versions[0].Info.HasPlayableMedia() {
				pr.detailTracksKnown = true
			}
			refs := s.variantRefs(t, versions)
			render := func(ids map[frontend.VariantRef]int64) []model.Media {
				return s.slotMedia(ctx, pr, t, versions, ids, info, ps, resumed, stamp, withStreams, batchDetail, selectedChapterMedia)
			}
			if deferSlots != nil && len(refs) != 0 {
				deferSlots(refs, render)
				return nil, versions[0].Edition, info
			}
			ids, err := s.variantIDsFor(ctx, refs)
			if err != nil {
				pr.versionErr = err
				return nil, "", info
			}
			return render(ids), versions[0].Edition, info
		}
	}
	if withVariants && withStreams && !sessionMedia {
		probed, probedRelease, playedSelector := frontend.MediaInfo{Estimated: true}, "", ""
		var playedRec *frontend.Release
		if ps != nil {
			// A client keeps the Media index it chose before playback
			// and reads Media[index] when the item is re-fetched during
			// the session (Plex Web after a subtitle download or a
			// timeline refresh). Collapsing the list to one Media leaves
			// that index pointing past the end, and Plex Web's player
			// settings throw on it. Keep the same version list and draw
			// the session's probe on the version actually playing.
			if ps.Resolution != nil {
				probedRelease = ps.Resolution.Release.ID
				playedSelector = ps.Resolution.VersionSelector
				relCopy := ps.Resolution.Release
				playedRec = &relCopy
			}
			if !info.Estimated && info.HasPlayableMedia() {
				probed = info
			}
		} else if resumed.ok {
			probed, probedRelease, playedSelector, info = resumed.info, resumed.rel.ID, resumed.selector, resumed.info
			relCopy := resumed.rel
			playedRec = &relCopy
		} else if !info.Estimated && info.HasPlayableMedia() {
			probed = info
		}
		var cachedAuto frontend.MediaInfo
		if media, edition := s.mediaForVariants(ctx, pr, t, stamp, probed, probedRelease, playedSelector, playedRec, ps, &cachedAuto); len(media) > 0 {
			return media, edition, info
		}
		if cachedAuto.HasPlayableMedia() {
			info, probedSource = cachedAuto, true
		}
	}
	m := buildMedia(t, info, stamp)
	if !withStreams && !sessionMedia && info.Estimated {
		var claim *frontend.Release
		if ps != nil && ps.Resolution != nil && ps.Resolution.Release.ID != "" && !strings.HasPrefix(ps.Resolution.Release.ID, frontend.VersionSelectorPrefix) {
			claim = &ps.Resolution.Release
		}
		unprobedListMedia(&m, t, claim)
	}
	// Detail pages and live sessions render with the ids this client was
	// issued (play_identity.go); /status/sessions names the Media the
	// session was admitted on. Batch metadata fetches do not allocate new
	// play_identity rows for unpinned items so library syncs never evict
	// active clients.
	var ik playIdentityKey
	if withStreams || ps != nil {
		cand := s.playIdentityFor(pr, t.Key)
		if !batchDetail || ps != nil || s.playIDs.has(cand) {
			ik = cand
			pin := s.playIDs.pinned(ik, ps != nil, s.now())
			if sessionMedia && ps != nil && pin.selected != 0 && pin.selected != int64(t.Key) {
				m.ID = pin.selected
				m.Part[0].ID = pin.selected
				m.Part[0].Key = mediaPartKey(pin.selected, stamp, info)
			}
			m.Part[0].Key = s.playIDs.partKey(ik, m.ID, m.Part[0].Key, s.now())
		}
	}
	if ps != nil && ps.Resolution != nil && s.trickplayAvailable(frontend.TrickplayCacheKey(ps)) {
		m.Part[0].Indexes = "sd"
	}
	if withStreams || probedSource {
		// Before a session the info is an estimate without subtitle
		// tracks: list the viewer's cached sidecars (OpenSubtitles) so
		// detail pages and play queues offer them.
		info = s.withCachedSidecars(ctx, pr.User, t, info)
		var userID int64
		if pr.User != nil {
			userID = pr.User.ID
		}
		stored := s.selections.get(userID, int64(t.Key))
		info, stored = subtitleInfoForPart(info, stored, m.Part[0].ID)
		sel := s.mediaTrackSelection(ctx, pr, t, ps, info, stored)
		m.Part[0].Stream = s.issuedStreams(t.Key, ik, m.ID, info, sel, pr.User, pr)
	}
	return []model.Media{m}, "", info
}

// partFileName is the virtual file name shown in "Get Info".
func partFileName(t *frontend.Title, ext string) string {
	name := t.Title
	switch t.Type {
	case frontend.TypeEpisode:
		name = fmt.Sprintf("%s - s%02de%02d - %s", t.ShowTitle, t.Season, t.Episode, t.Title)
	default:
		if t.Year > 0 {
			name = fmt.Sprintf("%s (%d)", t.Title, t.Year)
		}
	}
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\\:*?"<>|`, r) {
			return -1
		}
		return r
	}, name)
	return "/ether/" + name + "." + ext
}

func mediaFileExt(info frontend.MediaInfo) string {
	container := plexContainer(info.Container)
	switch {
	case strings.EqualFold(info.Container, "webm"):
		return "webm"
	case container == "mp4" && info.AudioOnly():
		return "m4a"
	}
	return containerExt(container)
}

func mediaPartKey(id, stamp int64, info frontend.MediaInfo) string {
	return fmt.Sprintf("/library/parts/%d/%d/file.%s", id, stamp, mediaFileExt(info))
}

func buildMedia(t *frontend.Title, info frontend.MediaInfo, stamp int64) model.Media {
	container := plexContainer(info.Container)
	dur := ms(info.Duration)
	if dur <= 0 {
		dur = ms(t.Runtime)
	}
	id := int64(t.Key)
	m := model.Media{
		ID:        id,
		Duration:  dur,
		Bitrate:   info.BitrateKbps,
		Container: container,
	}
	// PMS only reports the MP4 atoms for MP4/MOV files (an int on Media,
	// a bool on Part).
	isMP4 := container == "mp4" || container == "mov"
	if isMP4 {
		m.OptimizedForStreaming = model.Ptr(1)
		m.Has64bitOffsets = model.Ptr(false)
		if info.Has64bitOffsets != nil {
			m.Has64bitOffsets = info.Has64bitOffsets
		}
		if info.OptimizedForStreaming != nil {
			value := 0
			if *info.OptimizedForStreaming {
				value = 1
			}
			m.OptimizedForStreaming = &value
		}
	}
	var firstAudio *frontend.StreamInfo
	for i := range info.Streams {
		if info.Streams[i].Type == "audio" {
			if firstAudio == nil || info.Streams[i].Channels > firstAudio.Channels {
				firstAudio = &info.Streams[i]
			}
		}
	}
	if v := info.Video(); v != nil {
		m.Width, m.Height = v.Width, v.Height
		if v.Height > 0 {
			m.AspectRatio = plexAspectRatio(*v)
		}
		m.VideoCodec = plexCodec(v.Codec)
		m.VideoResolution = videoResolution(v.Width, v.Height)
		m.VideoFrameRate = frameRateName(v.FrameRate)
		m.VideoProfile = plexVideoProfile(v.Profile)
	}
	if firstAudio != nil {
		m.AudioChannels = firstAudio.Channels
		m.AudioCodec = plexCodec(firstAudio.Codec)
		m.AudioProfile = audioProfile(firstAudio)
		if m.AudioCodec == "dca" && m.AudioProfile == "ma" {
			m.AudioCodec = "dca-ma"
		}
	}
	m.HasVoiceActivity = model.Ptr(false)
	p := model.Part{
		ID:           id,
		Key:          mediaPartKey(id, stamp, info),
		Duration:     dur,
		File:         partFileName(t, mediaFileExt(info)),
		Size:         info.Size,
		AudioProfile: m.AudioProfile,
		Container:    container,
		VideoProfile: m.VideoProfile,
	}
	if container == "mpegts" {
		p.PacketLength = 188
	}
	if isMP4 {
		p.Has64bitOffsets = model.Ptr(false)
		p.OptimizedForStreaming = model.Ptr(true)
		if info.Has64bitOffsets != nil {
			p.Has64bitOffsets = info.Has64bitOffsets
		}
		if info.OptimizedForStreaming != nil {
			p.OptimizedForStreaming = info.OptimizedForStreaming
		}
	}
	m.Part = []model.Part{p}
	return m
}

// unprobedListMedia keeps only the facts Ether knows on a list row (section
// listings, hubs, On Deck) whose Media was drawn from an estimate. The
// estimate fills every attribute from the quality profile (2160p H.264 at
// 20 Mbps, E-AC-3 5.1, a default runtime), and clients that badge or pick a
// quality from the listing read those as the file's facts. The Media and
// Part elements stay (clients treat an item without Media as unplayable),
// with their ids, keys, file name and container; picture, sound, bitrate and
// size are reported only when the release Ether would play claims them in
// its name or size. claim is nil for a title without a picked release (a
// cold configured slot or the plain estimate). Detail pages, play queues and
// decisions are not list rows and keep their estimate until the probe lands.
func unprobedListMedia(m *model.Media, t *frontend.Title, claim *frontend.Release) {
	var info frontend.ReleaseInfo
	var size int64
	if claim != nil {
		info, size = claim.Info, claim.Size
	}
	dur := ms(t.Runtime)
	m.Duration = dur
	m.Bitrate = 0
	if size > 0 && t.Runtime > 0 {
		m.Bitrate = int(float64(size) * 8 / t.Runtime.Seconds() / 1000)
	}
	// Width and aspect ratio are never in a release name; height is.
	m.Width, m.AspectRatio = 0, 0
	m.Height, m.VideoResolution = 0, ""
	if info.Height > 0 {
		m.Height = info.Height
		m.VideoResolution = videoResolution(0, info.Height)
	}
	if info.VideoCodec != "" {
		m.VideoCodec = plexCodec(info.VideoCodec)
	} else {
		m.VideoCodec = ""
	}
	m.VideoFrameRate, m.VideoProfile = "", ""
	if info.AudioCodec != "" {
		m.AudioCodec = plexCodec(info.AudioCodec)
		m.AudioProfile = audioProfile(&frontend.StreamInfo{Codec: info.AudioCodec})
		if m.AudioCodec == "dca" && m.AudioProfile == "ma" {
			m.AudioCodec = "dca-ma"
		}
	} else {
		m.AudioCodec, m.AudioProfile = "", ""
	}
	m.AudioChannels = releaseChannelCount(info.Channels)
	m.OptimizedForStreaming, m.Has64bitOffsets = nil, nil
	for i := range m.Part {
		p := &m.Part[i]
		p.Duration = dur
		p.Size = size
		p.VideoProfile = ""
		p.AudioProfile = m.AudioProfile
		p.OptimizedForStreaming, p.Has64bitOffsets = nil, nil
	}
}

func releaseChannelCount(s string) int {
	s = strings.TrimSpace(s)
	switch s {
	case "":
		return 0
	case "7.1":
		return 8
	case "6.1":
		return 7
	case "5.1":
		return 6
	case "2.1":
		return 3
	case "2.0":
		return 2
	case "1.0":
		return 1
	}
	if a, b, ok := strings.Cut(s, "."); ok {
		ai, err1 := strconv.Atoi(a)
		bi, err2 := strconv.Atoi(b)
		if err1 == nil && err2 == nil && ai > 0 && bi >= 0 {
			return ai + bi
		}
	}
	return 0
}

// audioProfile is PMS's audioProfile ("lc" for AAC, "dts"/"ma" for DTS...).
func audioProfile(st *frontend.StreamInfo) string {
	if strings.HasPrefix(strings.ToLower(st.Codec), "pcm_") && st.Codec != "pcm_bluray" || st.Codec == "pcm" {
		return st.SampleFormat
	}
	if st.Profile != "" {
		p := strings.ToLower(st.Profile)
		if strings.Contains(p, "atmos") {
			return ""
		}
		switch p {
		case "dts-hd ma":
			return "ma"
		case "dts-hd hra":
			return "hra"
		case "dts", "dts core":
			return "dts"
		case "dts-es", "dts es":
			return "es"
		case "dts express", "dts-hd lbr":
			return "express"
		}
		return p
	}
	switch strings.ToLower(st.Codec) {
	case "aac":
		return "lc"
	case "dts":
		return "dts"
	case "dtshd", "dts-hd", "dts-hd ma":
		return "ma"
	case "dts-es":
		return "es"
	case "dts express", "dts-hd lbr":
		return "express"
	case "dts-hd hra":
		return "hra"
	}
	return ""
}

// buildStreams renders Stream elements and registers their ids.
func (s *Server) buildStreams(key frontend.RatingKey, info frontend.MediaInfo, sel selection, u *frontend.User) []model.Stream {
	out, _ := s.renderStreams(key, 0, info, sel, u)
	return out
}

// issuedStreams renders a Media's streams with the ids this client was
// already issued for that Media (see play_identity.go).
func (s *Server) issuedStreams(key frontend.RatingKey, ik playIdentityKey, mediaID int64, info frontend.MediaInfo, sel selection, u *frontend.User, requests ...*Request) []model.Stream {
	out, src := s.renderStreams(key, mediaID, info, sel, u)
	if ik.valid() && ik.key == key && s.playIDs != nil {
		s.playIDs.issueStreams(ik, mediaID, out, src, info, s.now())
	}
	if len(requests) > 0 && requests[0] != nil {
		projectStreamTitles(out, src, info, requests[0])
	}
	return out
}

// renderStreams also returns, per rendered stream, its index in info.Streams.
func (s *Server) renderStreams(key frontend.RatingKey, partID int64, info frontend.MediaInfo, sel selection, u *frontend.User) ([]model.Stream, []int) {
	if partID == int64(key) {
		partID = 0
	}
	var out []model.Stream
	var src []int
	audioSelected := -1
	if sel.audioSet {
		audioSelected = sel.audio
	}
	// Default audio: explicit selection, then preferred language, then the
	// default flag, then the first audio stream.
	if audioSelected < 0 {
		audioSelected = pickAudio(info, s.audioLanguages(u))
	}
	subSelected := -1
	if sel.subtitleSet {
		subSelected = sel.subtitle
	}
	subtitleSlots := make([]string, len(info.Streams))
	slotCounts := make(map[string]int)
	for i, st := range info.Streams {
		if st.Type == "subtitle" {
			slot := frontend.SubtitleDescriptorSlot(st)
			subtitleSlots[i] = slot
			slotCounts[slot]++
		}
	}
	for i, st := range info.Streams {
		id := s.assignStream(context.Background(), u, frontend.StreamBinding{
			Key: key, PartID: partID, Index: i, Info: st, Estimated: info.Estimated,
			DistinguishSubtitleIndex: slotCounts[subtitleSlots[i]] > 1,
		})
		ps := model.Stream{
			ID:               id,
			Codec:            plexCodec(st.Codec),
			Bitrate:          st.BitrateKbps,
			Title:            st.Title,
			StreamIdentifier: st.StreamIdentifier,
			Default:          boolPtrIf(st.Default),
			Forced:           boolPtrIf(st.Forced),
			Original:         trueDisposition(st.Original),
			Dub:              trueDisposition(st.Dub),
			Language:         languageName(st.Language),
		}
		if st.Language != "" {
			ps.LanguageCode = languageCode3(st.Language)
			ps.LanguageTag = languageTag(st.Language)
		}
		if !st.External {
			ps.Index = model.Ptr(st.Index)
		}
		switch st.Type {
		case "video":
			ps.StreamType = 1
			ps.Width, ps.Height = st.Width, st.Height
			ps.FrameRate = math.Round(st.FrameRate*1000) / 1000
			ps.BitDepth, ps.Level = st.BitDepth, st.Level
			if ps.Level < 0 {
				ps.Level = 0
			}
			ps.ColorRange = st.ColorRange
			ps.ChromaLocation, ps.CodedWidth, ps.CodedHeight = st.ChromaLocation, st.CodedWidth, st.CodedHeight
			if ps.Codec == "h264" && ps.CodedHeight <= 0 {
				ps.CodedHeight = st.Height
			}
			if ps.Codec == "h264" && ps.CodedHeight > 0 {
				ps.CodedHeight = ((ps.CodedHeight + 15) / 16) * 16
			}
			switch st.CodecTag {
			case "hev1", "hvc1", "dvhe", "XVID", "DX50", "HDMV":
				ps.CodecID = st.CodecTag
			}
			if n, d, ok := plexSAR(st.SampleAspectRatio); ok && n != d {
				ps.PixelAspectRatio = st.SampleAspectRatio
				delta := n - d
				if delta < 0 {
					delta = -delta
				}
				ps.Anamorphic = model.Ptr(delta*20 > d)
			}

			if st.FieldOrder != "" && st.FieldOrder != "unknown" {
				ps.ScanType = "progressive"
				if st.Interlaced {
					ps.ScanType = "interlaced"
				}
			}
			ps.Profile = plexVideoProfile(st.Profile)
			ps.ChromaSubsampling = "4:2:0"
			switch st.ColorTransfer {
			case "smpte2084", "arib-std-b67":
				ps.ColorTrc = st.ColorTransfer
				ps.ColorPrimaries = "bt2020"
				ps.ColorSpace = "bt2020nc"
			case "":
			default:
				ps.ColorTrc = st.ColorTransfer
			}
			if st.DolbyVision {
				ps.DOVIPresent = model.Ptr(true)
				ps.DOVIProfile, ps.DOVILevel = st.DolbyVisionProfile, st.DolbyVisionLevel
				if st.DolbyVisionProfile > 0 {
					ps.DOVIBLCompatID = model.Ptr(st.DolbyVisionCompatibilityID)
				}
				ps.DOVIBLPresent, ps.DOVIELPresent = st.DolbyVisionBLPresent, st.DolbyVisionELPresent
				ps.DOVIRPUPresent, ps.DOVIVersion = st.DolbyVisionRPUPresent, st.DolbyVisionVersion
			}
			ps.DisplayTitle = videoDisplayTitle(st)
			ps.ExtendedDisplayTitle = ps.DisplayTitle
		case "audio":
			ps.BitDepth = st.BitDepth
			if st.BitsPerRawSample != nil {
				ps.BitDepth = *st.BitsPerRawSample
			} else if ps.BitDepth == 0 && ps.Codec == "dca" && !strings.Contains(strings.ToLower(st.Profile), "express") {
				ps.BitDepth = 24
			}
			ps.HearingImpaired, ps.VisualImpaired, ps.Comment = trueDisposition(st.HearingImpaired), trueDisposition(st.VisualImpaired), trueDisposition(st.Commentary)
			ps.StreamType = 2
			ps.Channels = st.Channels
			ps.AudioChannelLayout = st.ChannelLayout
			ps.SamplingRate = st.SampleRate
			if st.Profile != "" || st.SampleFormat != "" {
				ps.Profile = audioProfile(&st)
			}
			if i == audioSelected {
				ps.Selected = model.Ptr(true)
			}
			ps.CanNormalizeLoudness = model.Ptr(false) // no loudness analysis
			ps.DisplayTitle = audioDisplayTitle(st)
			ps.ExtendedDisplayTitle = extendedTitle(st, ps.DisplayTitle, audioStreamCodecLabel(st))
		case "subtitle":
			ps.HearingImpaired, ps.VisualImpaired, ps.Comment = trueDisposition(st.HearingImpaired), trueDisposition(st.VisualImpaired), trueDisposition(st.Commentary)
			ps.StreamType = 3
			ps.CanAutoSync = model.Ptr(false) // no voice-activity data
			if st.External {
				ps.Key = "/library/streams/" + strconv.FormatInt(id, 10)
				ps.Format = ps.Codec
				ps.External = model.Ptr(true)
				ps.Transient = model.Ptr(0)
			}
			if i == subSelected {
				ps.Selected = model.Ptr(true)
			}
			ps.DisplayTitle = subtitleDisplayTitle(st)
			ps.ExtendedDisplayTitle = extendedTitle(st, ps.DisplayTitle, strings.ToUpper(plexCodec(st.Codec)))
		default:
			continue
		}
		out = append(out, ps)
		src = append(src, i)
	}
	return out, src
}

// extendedTitle is PMS's extendedDisplayTitle for audio and subtitle
// streams: the display title, or for a titled track "<title> (<language>
// <codec>)" ("Stereo (English DTS)", "SDH (English SRT)").
func extendedTitle(st frontend.StreamInfo, display, codec string) string {
	return composeTrackTitle(st, "", true)
}

// pickAudio chooses the audio stream for languages in preference order
// (see Server.audioLanguages), then the stream flagged default, then the
// first. Multi-language releases often flag a dub as default.
func pickAudio(info frontend.MediaInfo, langs []string) int {
	return frontend.PickAudio(info, langs)
}

func boolPtrIf(b bool) *bool {
	if b {
		return model.Ptr(true)
	}
	return nil
}

// plexCodec maps ffprobe/core codec names to the names PMS uses.
func plexCodec(c string) string {
	if strings.HasPrefix(strings.ToLower(c), "pcm_") && !strings.EqualFold(c, "pcm_bluray") {
		return "pcm"
	}
	switch strings.ToLower(c) {
	case "dts":
		return "dca"
	case "dtshd", "dts-hd", "dts-hd ma", "dts-es", "dts express", "dts-hd lbr", "dts-hd hra":
		return "dca"
	case "subrip":
		return "srt"
	case "hdmv_pgs_subtitle", "pgssub":
		return "pgs"
	case "dvd_subtitle", "dvdsub":
		return "vobsub"
	case "webvtt":
		return "vtt"
	case "h265":
		return "hevc"
	case "avc", "avc1":
		return "h264"
	case "e-ac3", "ec3":
		return "eac3"
	}
	return strings.ToLower(c)
}

// plexContainer maps container names to PMS's.
func plexContainer(c string) string {
	c = strings.ToLower(c)
	switch {
	case c == "":
		return "mkv"
	case strings.Contains(c, "matroska"), c == "mkv", c == "webm":
		return "mkv"
	case strings.Contains(c, "mp4"), c == "m4v":
		return "mp4"
	case c == "mov":
		return "mov"
	case c == "mpegts", c == "ts", c == "m2ts":
		return "mpegts"
	case c == "avi":
		return "avi"
	}
	return c
}

func containerExt(c string) string {
	switch c {
	case "mpegts":
		return "ts"
	case "":
		return "mkv"
	}
	return c
}

func plexVideoProfile(profile string) string {
	switch v := strings.ToLower(strings.TrimSpace(profile)); v {
	case "0", "unknown", "n/a":
		return ""
	case "simple profile":
		return "simple"
	default:
		return v
	}
}

func plexSAR(raw string) (int64, int64, bool) {
	a, b, ok := strings.Cut(raw, ":")
	if !ok {
		return 0, 0, false
	}
	n, e1 := strconv.ParseInt(a, 10, 32)
	d, e2 := strconv.ParseInt(b, 10, 32)
	return n, d, e1 == nil && e2 == nil && n > 0 && d > 0
}

func plexAspectRatio(v frontend.StreamInfo) float64 {
	if v.Width <= 0 || v.Height <= 0 {
		return 0
	}
	ratio := float64(v.Width) / float64(v.Height)
	if n, d, ok := plexSAR(v.SampleAspectRatio); ok {
		ratio *= float64(n) / float64(d)
	}
	best := 1.33
	for _, candidate := range []float64{1.66, 1.78, 1.85, 2.2, 2.35} {
		if math.Abs(candidate-ratio) < math.Abs(best-ratio) {
			best = candidate
		}
	}
	return best
}

// videoResolution is PMS's videoResolution value.
func videoResolution(w, h int) string {
	// These uncommon shapes have independently recorded nominal classes.
	// Keep unmeasured dimensions on the established general tier mapping.
	switch {
	case w == 1920 && (h == 1600 || h == 2160):
		return "2.7k"
	case w == 2048 && h == 858, w == 2560 && h == 1376:
		return "2k"
	case w == 1440 && h == 1048, w == 1852 && h == 800:
		return "720"
	}

	if w > 0 && w < 800 && h > 0 && h < 480 {
		return "sd"
	}
	switch tier := frontend.ResolutionTier(w, h); tier {
	case 2160:
		return "4k"
	case 0:
		return ""
	case 240:
		return "sd"
	default:
		return strconv.Itoa(tier)
	}
}

func resolutionLabel(w, h int) string {
	switch tier := videoResolution(w, h); tier {
	case "", "sd":
		return "SD"
	case "4k":
		return "4K"
	case "2k":
		return "2K"
	case "2.7k":
		return "2.7K"
	default:
		return tier + "p"
	}
}

// frameRateName is PMS's videoFrameRate value.
func frameRateName(f float64) string {
	switch {
	case f <= 0:
		return ""
	case f < 24.5:
		return "24p"
	case f < 25.5:
		return "PAL"
	case f < 30.5:
		return "NTSC"
	case f < 50.5:
		return "50p"
	default:
		return "60p"
	}
}

func videoDisplayTitle(st frontend.StreamInfo) string {
	res := resolutionLabel(st.Width, st.Height)
	if st.Interlaced {
		res = strings.TrimSuffix(res, "p") + "i"
	}
	parts := []string{}
	if st.DolbyVision {
		dv := "DoVi"
		if st.DolbyVisionELPresent != nil && *st.DolbyVisionELPresent && st.DolbyVisionBLPresent != nil && !*st.DolbyVisionBLPresent {
			dv = "DoVi Enhancement Layer"
		}
		parts = append(parts, dv)
	}
	switch st.ColorTransfer {
	case "smpte2084":
		parts = append(parts, "HDR10")
	case "arib-std-b67":
		parts = append(parts, "HLG")
	}
	if len(parts) > 0 {
		res += " " + strings.Join(parts, "/")
	}
	codec := strings.ToUpper(st.Codec)
	switch strings.ToLower(st.Codec) {
	case "h264":
		codec = "H.264"
	case "hevc":
		codec = "HEVC"
	}
	// PMS names the profile only where it matters ("HEVC Main 10"), not
	// for H.264 ("1080p (H.264)") or plain HEVC Main.
	if p := strings.ToLower(st.Profile); p != "" && p != "0" && p != "unknown" && (strings.ToLower(st.Codec) != "h264" || p != "main" && p != "high") {
		codec += " " + titleCase(st.Profile)
	}
	return res + " (" + codec + ")"
}

// audioCodecLabel is the codec as PMS writes it in stream titles.
func audioCodecLabel(c string) string {
	codec := strings.ToUpper(plexCodec(c))
	switch codec {
	case "DCA":
		return "DTS"
	case "DCA-MA":
		return "DTS-HD MA"
	case "TRUEHD":
		return "TRUEHD"
	}
	return codec
}

func audioStreamCodecLabel(st frontend.StreamInfo) string {
	if plexCodec(st.Codec) == "dca" && strings.Contains(strings.ToLower(st.Profile), "es") && !strings.Contains(strings.ToLower(st.Profile), "express") {
		return "DTS-ES"
	}
	if plexCodec(st.Codec) == "dca" && audioProfile(&st) == "ma" {
		return "DTS-HD MA"
	}
	return audioCodecLabel(st.Codec)
}

func titleCase(s string) string {
	words := strings.Fields(strings.ReplaceAll(s, "_", " "))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func channelsLabel(n int, layout string) string {
	switch n {
	case 1:
		return "Mono"
	case 2:
		return "Stereo"
	case 3:
		return "3.0"
	case 5:
		return "5.0"
	case 7:
		return "6.1"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	case 0:
		return ""
	}
	if layout != "" {
		label, _, _ := strings.Cut(layout, " (")
		return strings.TrimSpace(label)
	}
	return strconv.Itoa(n) + "ch"
}

func audioDisplayTitle(st frontend.StreamInfo) string {
	lang := languageName(st.Language)
	if lang == "" {
		lang = "Unknown"
	}
	inner := audioStreamCodecLabel(st)
	if ch := channelsLabel(st.Channels, st.ChannelLayout); ch != "" {
		if inner != "" {
			inner += " " + ch
		} else {
			inner = ch
		}
	}
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return lang
	}
	return lang + " (" + inner + ")"
}

func subtitleDisplayTitle(st frontend.StreamInfo) string {
	return composeTrackTitle(st, "", false)
}

// Languages: ISO 639-1 / 639-2 codes to names. Covers what releases and
// OpenSubtitles commonly carry; unknown codes pass through.
var languages = map[string][3]string{
	"ch":  {"Chamorro", "cha", "ch"},
	"gu":  {"Gujarati", "guj", "gu"},
	"hy":  {"Armenian", "hye", "hy"},
	"ka":  {"Georgian", "kat", "ka"},
	"mr":  {"Marathi", "mar", "mr"},
	"mdr": {"Mandar", "mdr", "mdr"},
	"mn":  {"Mongolian", "mon", "mn"},
	"my":  {"Burmese", "mya", "my"},
	"nn":  {"Norwegian Nynorsk", "nno", "nn"},
	"pa":  {"Punjabi", "pan", "pa"},
	"si":  {"Sinhala", "sin", "si"},
	"yo":  {"Yoruba", "yor", "yo"},
	"zu":  {"Zulu", "zul", "zu"},
	"fil": {"Filipino", "fil", "fil"},

	// code1: {name, code3(639-2/B), tag}
	"en": {"English", "eng", "en"}, "fr": {"French", "fre", "fr"}, "de": {"German", "ger", "de"},
	"es": {"Spanish", "spa", "es"}, "it": {"Italian", "ita", "it"}, "pt": {"Portuguese", "por", "pt"},
	"nl": {"Dutch", "dut", "nl"}, "sv": {"Swedish", "swe", "sv"}, "no": {"Norwegian", "nor", "no"},
	"nb": {"Norwegian Bokmål", "nob", "nb"}, "da": {"Danish", "dan", "da"}, "fi": {"Finnish", "fin", "fi"},
	"pl": {"Polish", "pol", "pl"}, "cs": {"Czech", "cze", "cs"}, "hu": {"Hungarian", "hun", "hu"},
	"ru": {"Russian", "rus", "ru"}, "uk": {"Ukrainian", "ukr", "uk"}, "el": {"Greek", "gre", "el"},
	"tr": {"Turkish", "tur", "tr"}, "ar": {"Arabic", "ara", "ar"}, "he": {"Hebrew", "heb", "he"},
	"hi": {"Hindi", "hin", "hi"}, "ja": {"Japanese", "jpn", "ja"}, "ko": {"Korean", "kor", "ko"},
	"zh": {"Chinese", "chi", "zh"}, "th": {"Thai", "tha", "th"}, "vi": {"Vietnamese", "vie", "vi"},
	"id": {"Indonesian", "ind", "id"}, "ro": {"Romanian", "rum", "ro"}, "bg": {"Bulgarian", "bul", "bg"},
	"hr": {"Croatian", "hrv", "hr"}, "sr": {"Serbian", "srp", "sr"}, "sk": {"Slovak", "slo", "sk"},
	"sl": {"Slovenian", "slv", "sl"}, "et": {"Estonian", "est", "et"}, "lv": {"Latvian", "lav", "lv"},
	"lt": {"Lithuanian", "lit", "lt"}, "fa": {"Persian", "per", "fa"}, "ms": {"Malay", "may", "ms"},
	"ta": {"Tamil", "tam", "ta"}, "te": {"Telugu", "tel", "te"}, "is": {"Icelandic", "ice", "is"},
	"ca": {"Catalan", "cat", "ca"}, "eu": {"Basque", "baq", "eu"}, "gl": {"Galician", "glg", "gl"},
}

// primaryLang is the display/code lookup key for a language: the
// canonical primary subtag from langtag (region and script dropped).
func primaryLang(code string) string {
	if t, ok := frontend.LanguagePrimary(code); ok {
		return canonicalLanguagePrimary(t)
	}
	return strings.ToLower(strings.TrimSpace(code))
}

// sameLanguage is the symmetric view of frontend.LanguageMatchTier: true when
// the tier is > 0 — exact, or the same primary with at least one
// generic side (pt × pt-br true; pt-br × pt-pt false).
func sameLanguage(a, b string) bool {
	return a != "" && b != "" && frontend.LanguageMatchTier(a, b) > 0
}

func languageName(code string) string {
	if code == "" || code == "und" {
		return ""
	}
	if l, ok := languages[primaryLang(code)]; ok {
		return nativeLanguage(l[2], l[0])
	}
	if canonicalLanguagePrimary(code) != code || unknownLanguageCode(code) != "" {
		return nativeLanguage(canonicalLanguagePrimary(code), code)
	}
	return code
}

func languageCode3(code string) string {
	if code == "und" || code == "" {
		return ""
	}
	if l, ok := languages[primaryLang(code)]; ok {
		return terminologyCode(l[2], l[1])
	}
	return unknownLanguageCode(code)
}

func languageTag(code string) string {
	if code == "und" || code == "" {
		return ""
	}
	if l, ok := languages[primaryLang(code)]; ok {
		return l[2]
	}
	return canonicalLanguagePrimary(strings.ToLower(code))
}

func renderTitleKeys(ts []frontend.Title) []frontend.RatingKey {
	lookup := make([]frontend.RatingKey, 0, len(ts)*2)
	for i := range ts {
		t := &ts[i]
		lookup = append(lookup, t.Key)
		if show := titleShow(t); show != 0 && show != t.Key {
			lookup = append(lookup, show)
		}
		if t.Type == frontend.TypeEpisode {
			if parent := titleParent(t); parent != 0 {
				lookup = append(lookup, parent)
			}
		}
	}
	return lookup
}

// renderTitles renders a list, loading the user's watch state in one call.
func (s *Server) renderTitles(ctx context.Context, pr *Request, ts []frontend.Title, o renderOpts) []model.Metadata {
	if len(ts) > 1 {
		o.listRender = true
	}
	if !reqmemo.Active(ctx) {
		ctx = reqmemo.With(ctx)
	}
	ctx = withStoredPolicy(ctx)
	if o.media && pr.excludeMedia && listMediaSkip && (!o.detail || o.skipExcludedMedia) && !o.detailEpisodes && !o.session {
		// The response drops Media (and mediaCount stands in for it), so
		// light list rows (and batch detail rows that do not ask for
		// chapters, markers, streams, or file checks) need not build it,
		// nor read what it is drawn from.
		o.media, o.mediaExcluded = false, true
	}
	// The rendering's own Playable checks (resumed tracks) read the same
	// series rows as admission.
	vis := s.visibilityCtx(ctx)
	if o.vis != nil {
		vis = *o.vis
	}
	ctx = s.withSeriesRows(ctx, pr.User, ts)
	if !o.membershipChecked {
		ts = s.openableTitlesWith(ctx, vis, pr.User, ts, o.availabilityInSQL || shelfContainsAll(ctx, ts))
	}
	if len(ts) == 0 {
		return nil
	}
	if !o.inSection && (o.section == nil || o.section.ID == 0) {
		// One membership read per candidate library for the page; each
		// row's metadata reads the attribution from the request memo.
		s.sectionsForTitles(ctx, ts)
	}
	batch, batched := ctx.Value(hubTitlePageKey{}).(hubTitlePage)
	s.warmTitleDetails(ts, o.storedOnly)
	if d := s.discovery(); d != nil {
		if d.Ratings != nil {
			ctx = d.Ratings.WithPage(ctx, ts)
		}
		if d.Overlays != nil && !batch.overlays {
			var viewer *frontend.User
			if pr != nil {
				viewer = pr.User
			}
			if next, err := d.Overlays.WithTitlePage(ctx, viewer, ts); err == nil {
				ctx = next
			} else {
				ctx = d.Overlays.WithSettingsSnapshot(ctx)
			}
		}
	}
	if o.state == nil {
		if batched && batch.state != nil {
			o.state = batch.state
		} else {
			o.state = s.stateFor(ctx, pr, ts)
		}
	}
	s.preloadUltraBlur(ctx, ts)
	ctx = s.preloadPageStreams(ctx, pr, ts, o)
	var guids map[frontend.RatingKey]string
	var missingGUIDs []frontend.RatingKey
	lookup := renderTitleKeys(ts)
	guidSrc := s.libraryGUIDs()
	if guidSrc != nil {
		if batched && !o.searchIdentity {
			guids = batch.guids
		} else {
			guids = guidSrc.CachedMany(ctx, lookup)
		}
		if o.searchIdentity {
			guids = resolveSearchGUIDs(ctx, guidSrc, ts, guids)
		}
	}
	ctx = s.withPlexMeta(ctx, lookup, guids, !o.storedOnly, o.searchIdentity)
	ctx = s.withPlexCredits(ctx, lookup, guids, o.detail && !o.storedOnly)
	var order episodeOrderView
	if !o.storedOnly {
		order = s.episodeOrderList(ctx, pr, ts, o.detail && !o.batchDetail)
	}
	if batched {
		o.slotPage = batch.slots
	} else if len(ts) > 1 {
		o.slotPage = &slotMediaPage{}
	}
	out := make([]model.Metadata, len(ts))
	for i := range ts {
		m := &out[i]
		s.metadataInto(ctx, pr, &ts[i], o, m)
		if !o.storedOnly {
			s.applyEpisodeOrder(ctx, pr, &ts[i], m, order)
		}
		if guidSrc != nil {
			k := ts[i].Key
			if guid := guids[k]; guid != "" {
				if !virtualPlexSeason(&ts[i]) {
					m.GUID = guid
				}
			} else {
				missingGUIDs = append(missingGUIDs, k)
			}
			switch ts[i].Type {
			case frontend.TypeSeason:
				if !virtualPlexSeason(&ts[i]) {
					if sg := guids[titleShow(&ts[i])]; sg != "" {
						m.ParentGUID = sg
					}
				}
			case frontend.TypeEpisode:
				if sg := guids[titleShow(&ts[i])]; sg != "" {
					m.GrandparentGUID = sg
				}
				if !strings.HasPrefix(m.ParentGUID, "local://episode-order/") {
					if seag := guids[titleParent(&ts[i])]; seag != "" {
						m.ParentGUID = seag
					}
				}
			}
		}
	}
	if !batched {
		s.finishSlotPage(ctx, pr, o.slotPage)
	}
	if guidSrc != nil && len(missingGUIDs) > 0 && !o.storedOnly {
		guidSrc.Schedule(missingGUIDs...)
	}
	if !o.detail && !o.skipLeafCounts {
		s.applyListLeafCountsOpts(ctx, pr, out, ts, o.storedOnly)
	}
	return out
}

type watchedEpisodesReader interface {
	WatchedEpisodes(ctx context.Context, userID int64) ([]frontend.RatingKey, error)
}

// watchedEpisodesOfReader reads only the given shows' watched episodes.
type watchedEpisodesOfReader interface {
	WatchedEpisodesOf(ctx context.Context, userID int64, shows []int64) ([]frontend.RatingKey, error)
}

func (s *Server) applyListLeafCountsOpts(ctx context.Context, pr *Request, items []model.Metadata, ts []frontend.Title, storedOnly bool) {
	if s.state() == nil || pr.User == nil || len(items) == 0 {
		return
	}
	var canonical, projected []frontend.Title
	for _, t := range ts {
		if t.Type != frontend.TypeShow && t.Type != frontend.TypeSeason {
			continue
		}
		if t.Key.Allocated() || !viewerEpisodeOrder(&t) {
			if !t.Key.IsPluginKey() {
				projected = append(projected, t)
			}
		} else {
			canonical = append(canonical, t)
		}
	}
	s.applyProjectedListLeafCounts(ctx, pr, items, projected)
	if len(canonical) == 0 {
		return
	}
	ts = canonical
	var watched []frontend.RatingKey
	var err error
	if reader, ok := s.state().(watchedEpisodesOfReader); ok {
		// Only the listed shows' history: a page never reads all of it.
		seen := map[int64]bool{}
		var shows []int64
		for i := range ts {
			tmdb := ts[i].IDs.TMDB
			if ts[i].Type == frontend.TypeSeason {
				tmdb = ts[i].ShowIDs.TMDB
			}
			if tmdb > 0 && !seen[tmdb] {
				seen[tmdb] = true
				shows = append(shows, tmdb)
			}
		}
		watched, err = reader.WatchedEpisodesOf(ctx, pr.User.ID, shows)
	} else if reader, ok := s.state().(watchedEpisodesReader); ok {
		watched, err = reader.WatchedEpisodes(ctx, pr.User.ID)
	} else {
		return
	}
	if err != nil || len(watched) == 0 {
		return
	}
	watchedByShow := make(map[frontend.RatingKey][]frontend.RatingKey)
	for _, k := range watched {
		if show := k.Parent().Parent(); show != 0 {
			watchedByShow[show] = append(watchedByShow[show], k)
		}
	}
	subset := make([]frontend.Title, 0)
	for i := range ts {
		if ts[i].Type != frontend.TypeShow && ts[i].Type != frontend.TypeSeason {
			continue
		}
		if len(watchedByShow[titleShow(&ts[i])]) > 0 {
			subset = append(subset, ts[i])
		}
	}
	if len(subset) == 0 {
		return
	}
	if !storedOnly {
		memo, states := s.prefetchListEpisodes(ctx, pr, subset)
		s.applyLeafCountsWith(ctx, pr, items, subset, memo, states)
	}
	byKey := make(map[string]int, len(items))
	for i := range items {
		byKey[items[i].RatingKey] = i
	}
	for _, title := range subset {
		i, ok := byKey[title.Key.String()]
		if !ok || (!storedOnly && items[i].ViewedLeafCount != nil && *items[i].ViewedLeafCount > 0) {
			continue
		}
		viewed := 0
		for _, k := range watchedByShow[titleShow(&title)] {
			switch title.Type {
			case frontend.TypeShow:
				if k.Season() > 0 {
					viewed++
				}
			case frontend.TypeSeason:
				if k.Season() == title.Season {
					viewed++
				}
			}
		}
		if viewed > 0 {
			items[i].ViewedLeafCount = model.Ptr(viewed)
		}
	}
}

// applyProjectedListLeafCounts uses stored counts for the page's alternate
// orders. It does not load the shows' layouts or visit metadata providers.
func (s *Server) applyProjectedListLeafCounts(ctx context.Context, pr *Request, items []model.Metadata, titles []frontend.Title) {
	catalogue, ok := s.catalog().(frontend.AiredEpisodeTallyReader)
	if !ok || len(titles) == 0 {
		return
	}
	seen := make(map[frontend.RatingKey]bool, len(titles))
	var shows []frontend.RatingKey
	for _, t := range titles {
		if show := titleShow(&t); show != 0 && !seen[show] {
			shows, seen[show] = append(shows, show), true
		}
	}
	var pending []frontend.RatingKey
	if reader, ok := s.state().(frontend.PendingWatchKeys); ok {
		pending = reader.PendingKeys(pr.User.ID)
	}
	tallies, err := catalogue.AiredEpisodeTallies(ctx, pr.User.ID, shows, s.now(), pending)
	if err != nil {
		return
	}
	var judged []frontend.RatingKey
	for _, tally := range tallies {
		judged = append(judged, tally.Judge...)
	}
	if reader, ok := s.catalog().(frontend.CachedTitleBatchReader); ok && len(judged) > 0 {
		rows, err := reader.CachedGetMany(ctx, judged)
		if err == nil {
			states, err := s.state().GetMany(ctx, pr.User.ID, judged)
			if err == nil {
				for key, t := range rows {
					tally := tallies[titleShow(&t)]
					if tally.Seasons == nil {
						continue
					}
					n := tally.Seasons[t.Season]
					n.Aired++
					if states[key].ViewCount > 0 {
						n.Played++
					}
					tally.Seasons[t.Season] = n
				}
			}
		}
	}
	byKey := make(map[string]*model.Metadata, len(items))
	for i := range items {
		byKey[items[i].RatingKey] = &items[i]
	}
	for _, t := range titles {
		m := byKey[t.Key.String()]
		tally, ok := tallies[titleShow(&t)]
		if !ok || m == nil {
			continue
		}
		watched := 0
		if t.Type == frontend.TypeSeason {
			watched = tally.Seasons[t.Season].Played
		} else {
			for season, counts := range tally.Seasons {
				if season > 0 {
					watched += counts.Played
				}
			}
		}
		m.ViewedLeafCount = model.Ptr(watched)
	}
}

func (s *Server) stateFor(ctx context.Context, pr *Request, ts []frontend.Title) map[frontend.RatingKey]frontend.WatchStatus {
	if s.state() == nil || pr.User == nil || len(ts) == 0 {
		return nil
	}
	keys := make([]frontend.RatingKey, 0, len(ts))
	seen := make(map[frontend.RatingKey]bool, len(ts))
	for i := range ts {
		if k := ts[i].Key; !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	st, err := reqmemo.Do(ctx, fmt.Sprintf("plex:state:%d:%v", pr.User.ID, keys), func() (map[frontend.RatingKey]frontend.WatchStatus, error) {
		return s.state().GetMany(ctx, pr.User.ID, keys)
	})
	if err != nil {
		s.log.Warn("load watch state", "err", err)
		return nil
	}
	copy := make(map[frontend.RatingKey]frontend.WatchStatus, len(st))
	for key, value := range st {
		copy[key] = value
	}
	return copy
}

// applyLeafCounts fills viewedLeafCount for shows and seasons by loading
// their episodes' watch state. Used on detail and children pages only. memo
// (may be nil) shares show episodes with the rest of the response.
func (s *Server) applyLeafCounts(ctx context.Context, pr *Request, items []model.Metadata, ts []frontend.Title, memo showEpisodes) {
	s.applyLeafCountsWith(ctx, pr, items, ts, memo, nil)
}

// prefetchListEpisodes loads, in a few batched reads, the episodes and
// watch state a list's shows need for their leaf counts, so the counts do
// not load each show's episodes and state one show at a time. It covers
// shows listed without their seasons whose episode order is the catalogue's
// (an episode group changes the episode set); the rest keep the per-show
// path. states holds every prefetched episode's watch state.
func (s *Server) prefetchListEpisodes(ctx context.Context, pr *Request, ts []frontend.Title) (showEpisodes, map[frontend.RatingKey]frontend.WatchStatus) {
	// Leaf counts need only episode keys and numbering when no title policy
	// can depend on hydrated fields. Unknown policies retain full metadata.
	light := pr.User != nil && pr.User.MaxContentRating == "" && !s.viewerRestricted(pr.User)
	if s.policy() != nil {
		policy, known := s.policy().(frontend.ArtworkAccessPolicy)
		light = light && known && !policy.Restricted(pr.User)
	}
	return s.prefetchListEpisodesWith(ctx, pr, ts, light)
}

func (s *Server) prefetchListEpisodesWith(ctx context.Context, pr *Request, ts []frontend.Title, selectionOnly bool) (showEpisodes, map[frontend.RatingKey]frontend.WatchStatus) {
	batchOpts := frontend.EpisodeBatchOptions{Catalog: s.catalog(), State: s.state()}
	if s.episodeOrder() != nil {
		batchOpts.EpisodeOrder = s.episodeOrder()
	}
	batch := s.exp().NewEpisodeBatch(batchOpts)
	found, states := batch.Load(ctx, pr.User, ts, selectionOnly)
	if found == nil {
		return nil, states
	}
	memo := make(showEpisodes, len(found))
	for show, eps := range found {
		memo[show] = showEpisodeList{eps: eps}
	}
	return memo, states
}

// applyLeafCountsWith is applyLeafCounts with prefetched watch state for the
// episodes of the shows memo already holds.
func (s *Server) applyLeafCountsWith(ctx context.Context, pr *Request, items []model.Metadata, ts []frontend.Title, memo showEpisodes, states map[frontend.RatingKey]frontend.WatchStatus) {
	if s.state() == nil || pr.User == nil {
		return
	}
	// Read all displayed containers' episode state once. This keeps the
	// pending-write overlay (fresh progress may not be flushed yet), while
	// avoiding a separate GetMany for each season.
	if memo == nil {
		memo = showEpisodes{}
	}
	coveredShows := map[frontend.RatingKey]bool{}
	if states != nil {
		for show, l := range memo {
			if l.err == nil {
				coveredShows[show] = true
			}
		}
	} else {
		seen := map[frontend.RatingKey]bool{}
		var all []frontend.Title
		complete := true
		for _, title := range ts {
			if title.Key.IsPluginKey() || (title.Type != frontend.TypeShow && title.Type != frontend.TypeSeason) {
				continue
			}
			show := titleShow(&title)
			if seen[show] {
				continue
			}
			seen[show] = true
			episodes, err := s.episodesOfShow(ctx, pr, memo, show)
			if err != nil {
				complete = false
				break
			}
			all = append(all, episodes...)
		}
		if complete {
			if len(all) > 0 {
				states = s.stateFor(ctx, pr, all)
			} else {
				states = map[frontend.RatingKey]frontend.WatchStatus{}
			}
			if states != nil {
				for show := range seen {
					coveredShows[show] = true
				}
			}
		}
	}
	byKey := map[string]int{}
	for i := range items {
		byKey[items[i].RatingKey] = i
	}
	// Seasons listed together (a show's children) are counted from their
	// show's episodes, loaded once, rather than by loading every season.
	together := map[frontend.RatingKey]int{}
	for i := range ts {
		if !ts[i].Key.IsPluginKey() && ts[i].Type == frontend.TypeSeason {
			together[titleShow(&ts[i])]++
		}
	}
	bySeason := map[frontend.RatingKey]map[int][]frontend.Title{}
	for sourceIndex := range ts {
		i, ok := byKey[ts[sourceIndex].Key.String()]
		if !ok {
			continue
		}
		title := ts[sourceIndex]
		if title.Key.IsPluginKey() {
			continue
		}
		var eps []frontend.Title
		var err error
		switch title.Type {
		case frontend.TypeShow:
			eps, err = s.episodesOfShow(ctx, pr, memo, title.Key)
		case frontend.TypeSeason:
			show := titleShow(&title)
			seasons, loaded := bySeason[show]
			if !loaded && together[show] > 1 {
				if all, e := s.episodesOfShow(ctx, pr, memo, show); e == nil {
					seasons = s.presentedSeasons(ctx, pr, show, all)
				}
				bySeason[show] = seasons
			}
			if seasons != nil {
				eps = seasons[title.Season]
			} else {
				// A lone season, or its show's episodes failed to load.
				eps, err = s.metadataChildren(ctx, pr, title.Key)
			}
		default:
			continue
		}
		if err != nil || len(eps) == 0 {
			continue
		}
		eps = s.visibleTitles(pr.User, eps)
		if title.Type == frontend.TypeShow {
			// A show's totals are its regular episodes, as the catalogue
			// counts them on every list: Specials neither add to the badge
			// nor keep a finished show unwatched.
			regular := eps[:0:0]
			for _, e := range eps {
				if e.Season > 0 {
					regular = append(regular, e)
				}
			}
			eps = regular
			if len(eps) == 0 {
				continue
			}
		}
		showKey := titleShow(&title)
		showCovered := coveredShows[showKey]
		if title.Type == frontend.TypeSeason && bySeason[showKey] == nil {
			showCovered = false
		}
		var st map[frontend.RatingKey]frontend.WatchStatus
		if states != nil && (showCovered || func() bool { _, ok := states[eps[0].Key]; return ok }()) {
			st = states
		} else {
			st = s.stateFor(ctx, pr, eps)
		}
		viewed := 0
		for _, e := range eps {
			if st[e.Key].ViewCount > 0 {
				viewed++
			}
		}
		// The episodes just counted are the ones the viewer can list and
		// play (aired, visible, in their episode order; regular seasons for
		// a show), so leafCount comes
		// from the same list as viewedLeafCount and a fully watched show
		// or season reads as watched.
		items[i].ViewedLeafCount = model.Ptr(viewed)
		items[i].LeafCount = model.Ptr(len(eps))

	}
}

// audioLanguages is the access-limit audio list, then the catalog language.
// Playback selection does not read it. That chain is trackprefs: the viewer's
// audioLanguages, otherwise user 0, then catalog.language, then the title
// original, then the file default flag.
func (s *Server) audioLanguages(u *frontend.User) []string {
	var langs []string
	if u != nil {
		langs = append(langs, u.AudioLanguages...)
	}
	l := ""
	if fn := s.getHost().MetadataLanguage; fn != nil {
		l = fn()
	}
	if l == "" {
		l = s.cfg().Catalog.Language
	}
	if l != "" {
		langs = append(langs, strings.SplitN(l, "-", 2)[0])
	}
	return langs
}

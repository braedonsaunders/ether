package app

import (
	"context"
	"io"
	"math"
	"strings"
	"time"

	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langfeat"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/plex"
	"github.com/braedonsaunders/ether/internal/subtitles"
)

// subtitleUser is u with the subtitle languages of the viewer's stream
// preferences (Viewers > Defaults or their override), which win over the
// account's own list.
func (a *App) subtitleUser(ctx context.Context, u *core.User) *core.User {
	if u == nil {
		return nil
	}
	copy := *u
	if a.trackprefs != nil {
		rules, err := a.trackprefs.GetRules(ctx, u.ID)
		if err == nil && len(rules.SubtitleLanguages) > 0 {
			copy.SubtitleLanguages = rules.SubtitleLanguages
		}
	}
	return &copy
}

// subtitlePrefs is the viewer's subtitle mode/style, used when listing
// sidecars already on disk. It does not cause a download.
func (a *App) subtitlePrefs(ctx context.Context, u *core.User) subtitles.Preferences {
	if u == nil || a.trackprefs == nil {
		return subtitles.Preferences{}
	}
	rules, err := a.trackprefs.GetRules(ctx, u.ID)
	if err != nil {
		return subtitles.Preferences{}
	}
	return subtitles.Preferences{Mode: rules.SubtitleMode, Style: rules.SubtitleStyle}
}

func (a *App) enrichSubtitles(ctx context.Context, t *core.Title, u *core.User, res *core.Resolution, src core.MediaSource, info *core.MediaInfo) {
	a.subtitles.Enrich(ctx, t, a.subtitleUser(ctx, u), res, src, info, a.subtitlePrefs(ctx, u))
}

// plexSubtitles is plex.Deps.Subtitles: the OpenSubtitles service with the
// viewer's preferred languages applied, as for playback enrichment.
type plexSubtitles struct{ a *App }

var (
	_ plex.SubtitleSource   = plexSubtitles{}
	_ plex.SubtitleCatalog  = plexSubtitles{}
	_ plex.SubtitleSearch   = plexSubtitles{}
	_ plex.SubtitleTimeline = plexSubtitles{}
	_ plex.SubtitleUpload   = plexSubtitles{}
)

func (p plexSubtitles) OpenFormat(id, format string) (io.ReadCloser, string, error) {
	return p.a.subtitles.OpenFormat(id, format)
}

func (p plexSubtitles) OpenFormatAt(id, format string, offset time.Duration) (io.ReadCloser, string, error) {
	return p.a.subtitles.OpenFormatAt(id, format, offset)
}

func (p plexSubtitles) CachedSidecars(ctx context.Context, t *core.Title, u *core.User, embedded []core.StreamInfo) []core.StreamInfo {
	out := p.a.subtitles.CachedSidecars(t, p.a.subtitleUser(ctx, u), embedded, p.a.subtitlePrefs(ctx, u))
	if !p.a.subtitles.Enabled() {
		out = append(out, p.a.subtitles.ManualSidecars(t, embedded)...)
	}
	return out
}

func (p plexSubtitles) SearchSubtitles(ctx context.Context, t *core.Title, _ *core.User, lang, release string) ([]plex.SubtitleOffer, error) {
	asked := lang
	cands, searchErr := p.a.subtitles.SearchOnDemand(ctx, t, lang, release)
	if searchErr != nil {
		cands = nil
	}
	out := make([]plex.SubtitleOffer, 0, len(cands))
	for _, c := range cands {
		if c.Language != "" {
			lang = c.Language
		}
		title := c.Release
		if title == "" {
			title = strings.TrimSuffix(c.FileName, ".srt")
		}
		out = append(out, plex.SubtitleOffer{
			ID: c.FileID, Language: lang, Title: title, Provider: "OpenSubtitles",
			Score: int(math.Round(c.Score)), HearingImpaired: c.HearingImp, Forced: c.Forced,
			PerfectMatch: c.HashMatch || (release != "" && strings.EqualFold(c.Release, release)),
		})
	}
	if p.a.lang != nil {
		for _, o := range p.a.lang.Offers(t, asked) {
			out = append(out, plex.SubtitleOffer{
				ID: o.ID, Language: o.Language, Title: o.Title, Provider: o.Provider,
			})
		}
	}
	if searchErr != nil && len(out) == 0 {
		return nil, searchErr
	}
	return out, nil
}

func (p plexSubtitles) DownloadSubtitle(ctx context.Context, t *core.Title, user *core.User, id int64) (core.StreamInfo, error) {
	if p.a.lang != nil && langfeat.IsReserved(id) {
		st, err := p.a.lang.Fulfill(ctx, user, t, id)
		if err == nil || !errors.Is(err, core.ErrNotFound) {
			return st, err
		}
	}
	st, err := p.a.subtitles.DownloadOnDemand(ctx, t, id)
	if err != nil {
		return st, err
	}
	p.a.subtitles.StartCorrection(t, st.ExternalURL, p.a.liveReleaseAccess(t, user))
	return st, nil
}

func (a *App) liveReleaseAccess(t *core.Title, u *core.User) subtitles.ReleaseAccess {
	sess := a.languageSession(u, t)
	if sess == nil || a.playback == nil {
		return subtitles.ReleaseAccess{}
	}
	src := sess.ID
	if sess.Resolution != nil && sess.Resolution.Release.ID != "" {
		src = sess.Resolution.Release.ID
	}
	return subtitles.ReleaseAccess{
		URL:      a.playback.InputURL(sess.ID),
		Duration: sess.Info.Duration,
		Info:     &sess.Info,
		Source:   src,
	}
}

func (p plexSubtitles) UploadSubtitle(_ context.Context, t *core.Title, _ *core.User, filename, format, language string, body []byte) (core.StreamInfo, error) {
	return p.a.subtitles.StoreUpload(t, filename, format, language, body)
}

// markerDialogue is markers.Options.Dialogue: cue times from subtitle text
// the system already holds for the played release (cached sidecars), for
// stinger detection. It never fetches video or touches the network.
func (a *App) markerDialogue(_ context.Context, _ core.RatingKey, info core.MediaInfo) ([]markers.DialogueCue, bool) {
	cues, ok := a.subtitles.CuesForInfo(info)
	if !ok {
		return nil, false
	}
	out := make([]markers.DialogueCue, len(cues))
	for i, c := range cues {
		out[i] = markers.DialogueCue{Start: c.Start, End: c.End}
	}
	return out, true
}

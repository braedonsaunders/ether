package playback

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type gateResolver struct{ blocked string }

func (g *gateResolver) Resolve(context.Context, core.ResolveRequest) (*core.Resolution, error) {
	return nil, core.ErrNotAvailable
}
func (g *gateResolver) MarkBad(context.Context, string, string) error { return nil }
func (g *gateResolver) Available(context.Context, core.RatingKey) (bool, bool) {
	return false, false
}
func (g *gateResolver) ReleaseBlocked(_ context.Context, id string) (bool, error) {
	return g.blocked != "" && g.blocked == id, nil
}

// The detail-page track list is the viewer's resume probe, not a shared cache.
// Another viewer, a profile-key change, a source-policy change, a block, and
// a new release each miss. The replacement release does not keep the old tracks.
func TestResumedTracksFollowTheResumeRow(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs, err := newResumeStore(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	profile := core.QualityProfile{Name: "1080p", MaxHeight: 1080, PreferredCodecs: []string{"h264"}}
	gates := &gateResolver{}
	m := &Manager{
		log:    slog.New(slog.DiscardHandler),
		now:    time.Now,
		resume: rs,
		deps: Deps{
			SelectProfile: func(core.ClientCaps, *core.User) core.QualityProfile { return profile },
			Resolver:      gates,
		},
	}
	owner := &core.User{ID: 1}
	other := &core.User{ID: 2}
	key := core.MovieKey(550)
	caps := core.ClientCaps{}
	french := core.MediaInfo{Container: "mkv", Size: 8000, Duration: time.Hour, Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Width: 3840, Height: 2160},
		{Index: 1, Type: "audio", Codec: "aac", Language: "fr", Channels: 2},
	}}
	if err := rs.putWithProbe(ctx, resumeRecord{
		userID: owner.ID, key: key, release: core.Release{ID: "rel-a", Protocol: core.ProtocolTorrent},
		profile: profileKey(profile), size: french.Size, duration: french.Duration, updatedAt: time.Now(),
	}, []byte("nzb-a"), nil, french); err != nil {
		t.Fatal(err)
	}

	info, rel, ok := m.ResumedTracks(ctx, owner, key, caps)
	if !ok || rel != "rel-a" || !hasAudioLang(info, "fr") {
		t.Fatalf("owner tracks = %+v %q %v", info.Streams, rel, ok)
	}
	if _, _, ok := m.ResumedTracks(ctx, other, key, caps); ok {
		t.Fatal("another viewer received the owner's release")
	}

	profile.MaxHeight = 2160
	if _, _, ok := m.ResumedTracks(ctx, owner, key, caps); ok {
		t.Fatal("profile change reused the old release")
	}
	profile.MaxHeight = 1080
	if _, rel, ok := m.ResumedTracks(ctx, owner, key, caps); !ok || rel != "rel-a" {
		t.Fatal("restored profile missed the resume row")
	}

	m.cfg.Resolver.SourcePreference = "usenet"
	if _, _, ok := m.ResumedTracks(ctx, owner, key, caps); ok {
		t.Fatal("source policy still served a torrent release")
	}
	m.cfg.Resolver.SourcePreference = ""

	gates.blocked = "rel-a"
	if _, _, ok := m.ResumedTracks(ctx, owner, key, caps); ok {
		t.Fatal("blocked release still served tracks")
	}
	gates.blocked = ""

	german := french
	german.Streams = []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Index: 1, Type: "audio", Codec: "eac3", Language: "de", Channels: 6},
	}
	if err := rs.putWithProbe(ctx, resumeRecord{
		userID: owner.ID, key: key, release: core.Release{ID: "rel-b"},
		profile: profileKey(profile), size: german.Size, duration: german.Duration, updatedAt: time.Now(),
	}, []byte("nzb-b"), nil, german); err != nil {
		t.Fatal(err)
	}
	info, rel, ok = m.ResumedTracks(ctx, owner, key, caps)
	if !ok || rel != "rel-b" || hasAudioLang(info, "fr") || !hasAudioLang(info, "de") {
		t.Fatalf("new release kept old tracks: %+v %q %v", info.Streams, rel, ok)
	}
}

func hasAudioLang(info core.MediaInfo, lang string) bool {
	for _, st := range info.Streams {
		if st.Type == "audio" && st.Language == lang {
			return true
		}
	}
	return false
}

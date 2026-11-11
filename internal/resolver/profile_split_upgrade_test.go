package resolver

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// A v0.3.4 quality section: profiles with no media type (they served
// movies and shows alike), device, platform and viewer rules, and
// separate At home and Away defaults. The profiles are already in the
// standard ranking shape, so only the split is under test.
const v034Quality = `{
  "profiles": [
    {"name": "4K", "maxHeight": 2160, "minHeight": 1080, "allowHdr": true, "allowDolbyVision": true,
     "qualityOrder": [{"name": "2160p", "qualities": ["remux-2160", "bluray-2160", "webdl-2160"]}, {"name": "1080p", "qualities": ["remux-1080", "bluray-1080", "webdl-1080"]}],
     "customFormatScores": {"ether:codec-hevc": 12, "ether:codec-h264": 6, "ether:audio-truehd": 12},
     "minSizeMbPerMin": 15, "maxSizeMbPerMin": 1000,
     "releaseRules": [{"id": "no-cam", "name": "No CAM", "action": "avoid",
       "conditions": [{"field": "name", "operator": "contains", "value": "CAM"}]}]},
    {"name": "1080p", "maxHeight": 1080, "minHeight": 720, "qualityOrder": [{"name": "1080p", "qualities": ["webdl-1080", "bluray-1080"]}],
     "customFormatScores": {"ether:codec-h264": 12, "ether:audio-eac3": 12},
     "minSizeMbPerMin": 5, "maxSizeMbPerMin": 150, "episodeMaxSizeMbPerMin": 80},
    {"name": "720p", "maxHeight": 720, "minHeight": 480, "qualityOrder": [{"qualities": ["webdl-720"]}],
     "customFormatScores": {"ether:audio-aac": 8}, "minSizeMbPerMin": 2, "maxSizeMbPerMin": 60},
    {"name": "Kids", "maxHeight": 1080, "minHeight": 480, "preferredLanguages": ["en"], "qualityOrder": [{"qualities": ["webdl-1080"]}],
     "minSizeMbPerMin": 2, "maxSizeMbPerMin": 80}
  ],
  "rules": [
    {"product": "Apple TV", "profile": "4K"},
    {"platform": "iOS", "profile": "720p", "maxHeight": 720},
    {"userId": 7, "profile": "Kids", "convertSurroundAudio": true},
    {"product": "Roku", "profile": "1080p", "maxHeight": 1080}
  ],
  "default": "1080p",
  "remote": "720p"
}`

// The one-time split of shared profiles must leave every viewer, client
// and title on exactly the settings v0.3.4 gave them: for movies, for
// shows and before the media is known; per client rule; at home and away.
func TestSplitSharedProfilesKeepsEveryChoice(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"quality": `+v034Quality+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read decodes exactly as Open does but never upgrades: v0.3.4's view.
	stored, err := config.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := stored.Quality
	if len(before.Profiles) != 4 || before.Profiles[0].MediaType != core.MediaAny {
		t.Fatalf("baseline is not the v0.3.4 shape: %+v", before.Profiles)
	}
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	after := store.Get().Quality

	// Every profile now serves exactly one media, as a movie copy followed
	// by a TV copy of the same name; references are untouched.
	if len(after.Profiles) != 2*len(before.Profiles) {
		t.Fatalf("profiles %d, want %d", len(after.Profiles), 2*len(before.Profiles))
	}
	for i, p := range before.Profiles {
		for j, media := range core.ProfileMediaTypes {
			got := after.Profiles[2*i+j]
			if got.Name != p.Name || got.MediaType != media {
				t.Fatalf("profile %d = %q/%q, want %q/%q", 2*i+j, got.Name, got.MediaType, p.Name, media)
			}
		}
	}
	if !reflect.DeepEqual(after.Rules, before.Rules) || after.Default != before.Default || after.Remote != before.Remote ||
		after.DefaultMovie != "" || after.DefaultTV != "" || after.RemoteMovie != "" || after.RemoteTV != "" {
		t.Fatalf("references rewritten: %+v", after)
	}

	viewers := map[string]*core.User{"nobody": nil, "owner": {ID: 1}, "kid": {ID: 7}}
	clients := map[string]core.ClientCaps{
		"apple tv": appleTVCaps(),
		"iphone":   {Product: "Plex for iOS", Platform: "iOS", MaxHeight: 1080, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"}},
		"roku":     {Product: "Roku", MaxHeight: 2160, VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3"}},
		"web":      webCaps(),
		"android":  {Product: "Plex for Android (Mobile)", Platform: "Android", VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3"}},
	}
	titles := map[string]*core.Title{
		"movie":   {Key: core.MovieKey(603), Type: core.TypeMovie},
		"episode": {Key: core.EpisodeKey(2316, 1, 2), Type: core.TypeEpisode},
	}
	shared := func(p core.QualityProfile) core.QualityProfile {
		p.MediaType = core.MediaAny
		return p
	}
	checked := 0
	for vn, user := range viewers {
		for cn, base := range clients {
			for _, remote := range []bool{false, true} {
				caps := base
				caps.Remote = remote
				for _, media := range []string{core.MediaAny, core.MediaMovie, core.MediaTV} {
					at := fmt.Sprintf("%s on %s, remote=%v, media=%q", vn, cn, remote, media)
					was, now := chooseProfileForMedia(before, caps, user, media, ""), chooseProfileForMedia(after, caps, user, media, "")
					if !reflect.DeepEqual(shared(now.profile), shared(was.profile)) || now.source != was.source || now.rule != was.rule {
						t.Fatalf("%s: profile %q (%s %d), was %q (%s %d)", at, now.profile.Name, now.source, now.rule, was.profile.Name, was.source, was.rule)
					}
					// The profile as clamped to the client, the one ranking uses.
					if got, want := decideProfile(after, caps, user, media, "").profile, decideProfile(before, caps, user, media, "").profile; !reflect.DeepEqual(shared(got), shared(want)) {
						t.Fatalf("%s: selected %+v, was %+v", at, got, want)
					}
					if got, want := ApplyClientRulesForMedia(after, caps, user, media), ApplyClientRulesForMedia(before, caps, user, media); !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: caps %+v, was %+v", at, got, want)
					}
					checked++
				}
				for tn, title := range titles {
					at := fmt.Sprintf("%s on %s, remote=%v, %s", vn, cn, remote, tn)
					if got, want := ApplyClientRulesForTitle(after, caps, user, title), ApplyClientRulesForTitle(before, caps, user, title); !reflect.DeepEqual(got, want) {
						t.Fatalf("%s: caps %+v, was %+v", at, got, want)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no choices compared")
	}

	// The split is saved once: reopening finds nothing left to split.
	again, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Get().Quality.Profiles; !reflect.DeepEqual(got, after.Profiles) {
		t.Fatalf("reopened profiles %+v", got)
	}
}

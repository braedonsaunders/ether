package plextv

import (
	"context"
	"testing"
	"time"
)

func TestEtherSubtitleMode(t *testing.T) {
	if EtherSubtitleMode(0) != "off" || EtherSubtitleMode(1) != "foreign-only" || EtherSubtitleMode(2) != "always" {
		t.Fatal("plex.tv subtitle modes")
	}
	if (TrackDefaults{SubtitleMode: 0, ForcedOnly: true}).EtherMode() != "forced-only" {
		t.Fatal("forced-only")
	}
	if (TrackDefaults{SubtitleMode: 2, ForcedOnly: true}).EtherMode() != "always" {
		t.Fatal("always wins over forced")
	}
}

func TestTrackDefaultsFromJSON(t *testing.T) {
	d := trackDefaultsFromJSON([]byte(`{"defaultAudioLanguage":"ja","autoSelectAudio":true,"defaultSubtitleLanguage":"en","autoSelectSubtitle":2}`))
	if !d.HasProfile || d.AudioLanguage != "ja" || !d.AutoSelectAudio || d.SubtitleLanguage != "en" || d.EtherMode() != "always" {
		t.Fatalf("%+v", d)
	}
	nested := trackDefaultsFromJSON([]byte(`{"profile":{"defaultAudioLanguage":"fr","autoSelectAudio":true,"defaultSubtitleLanguage":"de","autoSelectSubtitle":1,"defaultSubtitleForced":true}}`))
	if nested.AudioLanguage != "fr" || nested.EtherMode() != "foreign-only" || nested.ForcedOnly {
		t.Fatalf("prefer-forced must not turn subtitles on: %+v", nested)
	}
	if trackDefaultsFromJSON([]byte(`{}`)).HasProfile {
		t.Fatal("empty JSON is not a profile")
	}
}

func TestTrackDefaultsFromPlexTVUserJSON(t *testing.T) {
	// Shape of GET /api/v2/user: defaultSubtitleForced is a number, languages
	// also arrive as the plural CSV fields.
	raw := []byte(`{
		"id": 42,
		"username": "owner",
		"title": "The Owner",
		"profile": {
			"autoSelectAudio": true,
			"autoSelectSubtitle": 0,
			"defaultAudioLanguage": "",
			"defaultAudioLanguages": "ja,en",
			"defaultSubtitleLanguage": "",
			"defaultSubtitleLanguages": "en,fr",
			"defaultSubtitleForced": 2
		}
	}`)
	d := trackDefaultsFromJSON(raw)
	if !d.HasProfile || d.AudioLanguage != "ja" || !d.AutoSelectAudio || d.SubtitleLanguage != "en" || d.EtherMode() != "forced-only" {
		t.Fatalf("%+v", d)
	}
	prefer := trackDefaultsFromJSON([]byte(`{"profile":{"autoSelectSubtitle":0,"defaultSubtitleForced":1,"defaultAudioLanguage":"en"}}`))
	if prefer.EtherMode() != "off" || prefer.ForcedOnly {
		t.Fatalf("forced=1 must stay off: %+v", prefer)
	}
}

func TestUserKeepsFirstDecodeWhenProfileJSONUsesNumbers(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("owner-device", owner())
	f.userJSON = `{
		"id": 42,
		"uuid": "owner-uuid",
		"username": "owner",
		"title": "The Owner",
		"profile": {
			"autoSelectAudio": true,
			"autoSelectSubtitle": 1,
			"defaultAudioLanguage": "en",
			"defaultAudioLanguages": "en,ja",
			"defaultSubtitleLanguage": "fr",
			"defaultSubtitleLanguages": "fr",
			"defaultSubtitleForced": 2
		}
	}`
	c, err := New(Options{BaseURL: f.srv.URL, Identity: ServerIdentity(testConfig(), ""), RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.User(context.Background(), "owner-device")
	if err != nil || got.Track.AudioLanguage != "en" || got.Track.SubtitleLanguage != "fr" || got.Track.EtherMode() != "foreign-only" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestFriendsParseProfileAndShare(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("server-token-42", owner())
	f.accessXML = `<?xml version="1.0" encoding="UTF-8"?>
<tokens>
  <token token="friend-a" owned="0" username="alice" id="100">
    <invited id="100" username="alice" title="Alice">
      <profile defaultAudioLanguage="ja" autoSelectAudio="1" defaultSubtitleLanguage="en" autoSelectSubtitle="1" defaultSubtitleForced="0"/>
    </invited>
    <settings filterMovies="contentRating&lt;=PG-13" filterTelevision="label!=Hidden" allowSync="0"/>
  </token>
</tokens>`
	c := newServerClient(t, f)
	friends, err := c.AccessTokens(context.Background(), "server-token-42")
	if err != nil || len(friends) != 1 {
		t.Fatalf("%+v %v", friends, err)
	}
	got := friends[0]
	if got.Track.AudioLanguage != "ja" || !got.Track.AutoSelectAudio || got.Track.SubtitleLanguage != "en" || got.Track.EtherMode() != "foreign-only" {
		t.Fatalf("track %+v", got.Track)
	}
	if got.Share.FilterMovies != "contentRating<=PG-13" || got.Share.FilterTelevision != "label!=Hidden" || got.Share.AllowSync == nil || *got.Share.AllowSync {
		t.Fatalf("share %+v", got.Share)
	}
}

func TestSharedServersParseShare(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	f.addUser("owner-token", owner())
	f.sharedXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer>
  <SharedServer username="carol" userID="200" accessToken="friend-c" acceptedAt="1" allowSync="0" filterMovies="contentRating=G" filterTelevision="contentRating&lt;=TV-PG"/>
</MediaContainer>`
	c := newServerClient(t, f)
	friends, err := c.SharedServers(context.Background(), "owner-token")
	if err != nil || len(friends) != 1 {
		t.Fatalf("%+v %v", friends, err)
	}
	s := friends[0].Share
	if s.FilterMovies != "contentRating=G" || s.FilterTelevision != "contentRating<=TV-PG" || s.AllowSync == nil || *s.AllowSync {
		t.Fatalf("%+v", s)
	}
}

func TestUserParsesProfile(t *testing.T) {
	f := newFakePlexTV(t, testMachineID)
	acct := owner()
	acct.Track = TrackDefaults{AudioLanguage: "en", AutoSelectAudio: true, SubtitleLanguage: "fr", SubtitleMode: 2, HasProfile: true}
	f.addUser("owner-device", acct)
	c, err := New(Options{BaseURL: f.srv.URL, Identity: ServerIdentity(testConfig(), ""), RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.User(context.Background(), "owner-device")
	if err != nil || got.Track.AudioLanguage != "en" || !got.Track.AutoSelectAudio || got.Track.SubtitleLanguage != "fr" || got.Track.EtherMode() != "always" {
		t.Fatalf("%+v %v", got, err)
	}

	f.userXML = `<?xml version="1.0"?><user id="42" username="owner"><profile defaultAudioLanguage="ja" autoSelectAudio="1" defaultSubtitleLanguage="en" autoSelectSubtitle="0" defaultSubtitleForced="2"/></user>`
	got, err = c.User(context.Background(), "owner-device")
	if err != nil || got.Track.AudioLanguage != "ja" || got.Track.EtherMode() != "forced-only" {
		t.Fatalf("xml %+v %v", got, err)
	}
}

package livetv

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestDedupeProgrammesKeepsLast(t *testing.T) {
	at := time.Unix(100, 0)
	got := dedupeProgrammes([]Programme{
		{ChannelID: "a", Start: at, Item: Item{Title: "first"}},
		{ChannelID: "a", Start: at, Item: Item{Title: "second"}},
		{ChannelID: "a", Start: at.Add(time.Hour), Item: Item{Title: "later"}},
		{ChannelID: "b", Start: at, Item: Item{Title: "other"}},
	})
	if len(got) != 3 {
		t.Fatalf("got %d programmes: %+v", len(got), got)
	}
	if got[0].Item.Title != "second" || got[1].Item.Title != "later" || got[2].Item.Title != "other" {
		t.Fatalf("unexpected order: %+v", got)
	}
}

func TestParseM3USkipsVOD(t *testing.T) {
	m3u := `#EXTM3U
#EXTINF:-1,Movie One
https://owner.invalid/movie/user/pass/1.mp4
#EXTINF:-1,Series One
https://owner.invalid/series/user/pass/2.mp4
#EXTINF:-1 tvg-id="live.one" tvg-name="Live One Provider",Live One
https://owner.invalid/live/user/pass/3.ts
#EXTINF:-1,Shift One
https://owner.invalid/timeshift/user/pass/4.ts
#EXTINF:-1,Plain One
https://owner.invalid/user/pass/5.m3u8
#EXTINF:-1,Movie In Query
https://owner.invalid/live/user/pass/6.ts?next=/movie/user/pass/7.mp4
#EXTINF:-1,Short Movie Path
https://owner.invalid/movie/only.mp4
#EXTINF:-1,Upper Movie
https://owner.invalid/MOVIE/user/pass/8.mp4
`
	cs, stats, err := parseM3U(strings.NewReader(m3u), "source")
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedVOD != 3 {
		t.Fatalf("skipped %d VOD entries, want 3", stats.SkippedVOD)
	}
	want := []string{"Live One", "Shift One", "Plain One", "Movie In Query", "Short Movie Path"}
	if len(cs) != len(want) {
		t.Fatalf("kept %d channels, want %d: %+v", len(cs), len(want), cs)
	}
	for i, name := range want {
		if cs[i].Name != name {
			t.Fatalf("channel %d = %q, want %q", i, cs[i].Name, name)
		}
	}
	if cs[0].GuideID != "live.one" || cs[0].TvgName != "Live One Provider" {
		t.Fatalf("live metadata lost: %+v", cs[0])
	}
	// The exported wrapper keeps its old signature and the same filtering.
	wrapped, err := ParseM3U(strings.NewReader(m3u), "source")
	if err != nil || len(wrapped) != len(cs) {
		t.Fatalf("wrapper: %v %+v", err, wrapped)
	}
}

func TestImportRecordsSkippedVOD(t *testing.T) {
	ctx := context.Background()
	s := testService(t, Options{Now: func() time.Time { return guideTestNow }})
	src := Source{ID: "vod", Name: "VOD", Playlist: "#EXTM3U\n" +
		"#EXTINF:-1,Movie\nhttps://owner.invalid/movie/user/pass/1.mp4\n" +
		"#EXTINF:-1,Series\nhttps://owner.invalid/series/user/pass/2.mp4\n" +
		"#EXTINF:-1 tvg-id=\"one\",One\nhttps://owner.invalid/live/user/pass/3.ts\n"}
	got, err := s.ImportSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if got.SkippedVOD != 2 || got.ChannelCount != 1 {
		t.Fatalf("import stats: %+v", got)
	}
	if persisted := sourceByID(t, s, "vod"); persisted.SkippedVOD != 2 {
		t.Fatalf("skipped count not persisted: %+v", persisted)
	}
	src.Playlist = "#EXTM3U\n#EXTINF:-1 tvg-id=\"one\",One\nhttps://owner.invalid/live/user/pass/3.ts\n"
	if _, err = s.ImportSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if persisted := sourceByID(t, s, "vod"); persisted.SkippedVOD != 0 {
		t.Fatalf("skipped count not reset: %+v", persisted)
	}
}

func TestGuideLateChannelsCounted(t *testing.T) {
	guide := `<?xml version="1.0"?><tv>
<channel id="keep"><display-name>Keep</display-name></channel>
<programme channel="keep" start="20261019120000 +0000" stop="20261019130000 +0000"><title>x</title></programme>
<channel id="late-allowed"><display-name>Late Allowed</display-name></channel>
<channel id="late-dropped"><display-name>Late Dropped</display-name></channel>
<programme channel="late-allowed" start="20261019120000 +0000" stop="20261019130000 +0000"><title>y</title></programme>
</tv>`
	ids := map[string]struct{}{"keep": {}, "late-allowed": {}}
	g, _, err := parseXMLTV(strings.NewReader(guide), guideFilter{IDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	if g.LateChannels != 2 {
		t.Fatalf("late channels = %d, want 2", g.LateChannels)
	}
	if _, ok := g.Channels["late-allowed"]; !ok {
		t.Fatalf("allowed late channel was dropped: %+v", g.Channels)
	}
	if _, ok := g.Channels["late-dropped"]; ok {
		t.Fatalf("unlisted late channel was kept: %+v", g.Channels)
	}
	if len(g.Programmes) != 2 {
		t.Fatalf("programmes: %+v", g.Programmes)
	}
}

func TestParseXMLTVResolvesAtEOFWithoutProgrammes(t *testing.T) {
	guide := `<?xml version="1.0"?><tv>
<channel id="keep"><display-name>Keep</display-name></channel>
<channel id="candidate"><display-name>Candidate</display-name></channel>
<channel id="dropped"><display-name>Dropped</display-name></channel>
</tv>`
	ids := map[string]struct{}{"keep": {}}
	var resolved bool
	g, dropped, err := parseXMLTV(strings.NewReader(guide), guideFilter{
		IDs: ids,
		Resolve: func(channels map[string]Channel, names map[string][]string) map[string]struct{} {
			resolved = true
			if len(channels) != 3 {
				t.Errorf("resolver saw %d channels, want all 3", len(channels))
			}
			return map[string]struct{}{"candidate": {}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved || !dropped || len(g.Channels) != 2 || g.Channels["candidate"].Name != "Candidate" {
		t.Fatalf("EOF resolution: resolved=%v dropped=%v channels=%+v", resolved, dropped, g.Channels)
	}
	if _, ok := g.Channels["dropped"]; ok {
		t.Fatalf("unresolved channel kept: %+v", g.Channels)
	}
	if len(ids) != 1 {
		t.Fatalf("caller's allow map was mutated: %+v", ids)
	}
}

func TestVODExclusionRequiresXtreamShape(t *testing.T) {
	for _, url := range []string{"https://cdn.example/movie/hls/master.m3u8", "https://cdn.example/movie/hls/hd/master.m3u8", "https://cdn.example/series/u/p/12.ts/extra", "https://cdn.example/movie//p/12.mp4"} {
		if isVODStream(url) {
			t.Errorf("excluded live URL %s", url)
		}
	}
	for _, url := range []string{"https://cdn.example/movie/u/p/12.mp4", "https://cdn.example/prefix/series/u/p/12.mkv"} {
		if !isVODStream(url) {
			t.Errorf("kept VOD URL %s", url)
		}
	}
}

func TestParseXMLTVRichProgrammeMetadata(t *testing.T) {
	guide := `<?xml version="1.0"?><tv>
<channel id="ch1"><display-name>Channel 1</display-name></channel>
<programme channel="ch1" start="20261019120000 +0000" stop="20261019130000 +0000">
  <title>The Great Show</title>
  <sub-title>Episode of Wonder</sub-title>
  <desc>A wonderful episode of the show.</desc>
  <category>Drama</category>
  <category>Sci-Fi</category>
  <date>20240510</date>
  <episode-num system="xmltv_ns">2 . 4 .</episode-num>
  <new/>
  <icon src="https://images.example.com/poster.jpg"/>
  <icon src="https://images.example.com/backdrop.jpg"/>
</programme>
<programme channel="ch1" start="20261019130000 +0000" stop="20261019140000 +0000">
  <title>Live Sports</title>
  <desc>Championship game.</desc>
  <live/>
  <premiere/>
  <episode-num system="onscreen">2x05</episode-num>
</programme>
</tv>`
	g, err := ParseXMLTV(strings.NewReader(guide))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Programmes) != 2 {
		t.Fatalf("expected 2 programmes, got %d", len(g.Programmes))
	}
	p := g.Programmes[0]
	if p.Item.Title != "The Great Show" {
		t.Errorf("title = %q, want %q", p.Item.Title, "The Great Show")
	}
	if p.Item.EpisodeTitle != "Episode of Wonder" {
		t.Errorf("episodeTitle = %q, want %q", p.Item.EpisodeTitle, "Episode of Wonder")
	}
	if p.Item.ShowTitle != "The Great Show" {
		t.Errorf("showTitle = %q, want %q", p.Item.ShowTitle, "The Great Show")
	}
	if p.Item.MediaType != "episode" {
		t.Errorf("mediaType = %q, want %q", p.Item.MediaType, "episode")
	}
	if p.Item.Season != 3 || p.Item.Episode != 5 {
		t.Errorf("season/episode = %d/%d, want 3/5", p.Item.Season, p.Item.Episode)
	}
	if len(p.Item.Genres) != 2 || p.Item.Genres[0] != "Drama" || p.Item.Genres[1] != "Sci-Fi" {
		t.Errorf("genres = %v, want [Drama, Sci-Fi]", p.Item.Genres)
	}
	expectedDate, _ := time.Parse("20060102", "20240510")
	if p.Item.ReleasedMS != expectedDate.UnixMilli() {
		t.Errorf("releasedMS = %d, want %d", p.Item.ReleasedMS, expectedDate.UnixMilli())
	}
	if p.Item.Thumb != "https://images.example.com/poster.jpg" {
		t.Errorf("thumb = %q, want poster.jpg", p.Item.Thumb)
	}
	if p.Item.Art != "https://images.example.com/backdrop.jpg" {
		t.Errorf("art = %q, want backdrop.jpg", p.Item.Art)
	}
	if !p.Item.Premiere {
		t.Errorf("premiere = false, want true")
	}

	p2 := g.Programmes[1]
	if !p2.Item.Live {
		t.Errorf("p2 live = false, want true")
	}
	if !p2.Item.Premiere {
		t.Errorf("p2 premiere = false, want true")
	}
	if p2.Item.Season != 2 || p2.Item.Episode != 5 {
		t.Errorf("p2 season/episode = %d/%d, want 2/5", p2.Item.Season, p2.Item.Episode)
	}
}

func TestParseXMLEpisodePrecedenceAndPatterns(t *testing.T) {
	// xmltv_ns takes precedence even when preceded by dd_progid TMS identifiers
	guide := `<?xml version="1.0"?><tv>
<channel id="ch1"><display-name>Channel 1</display-name></channel>
<programme channel="ch1" start="20261019120000 +0000" stop="20261019130000 +0000">
  <title>Simpsons</title>
  <episode-num system="dd_progid">EP01085588.0019</episode-num>
  <episode-num system="xmltv_ns">1 . 18 .</episode-num>
  <episode-num system="onscreen">S02E19</episode-num>
</programme>
<programme channel="ch1" start="20261019130000 +0000" stop="20261019140000 +0000">
  <title>Sports Event</title>
  <episode-num system="onscreen">Game 7</episode-num>
</programme>
<programme channel="ch1" start="20261019140000 +0000" stop="20261019150000 +0000">
  <title>Drama Series</title>
  <episode-num system="onscreen">Season 3 Episode 4</episode-num>
</programme>
</tv>`
	g, err := ParseXMLTV(strings.NewReader(guide))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Programmes) != 3 {
		t.Fatalf("expected 3 programmes, got %d", len(g.Programmes))
	}
	p1 := g.Programmes[0]
	if p1.Item.Season != 2 || p1.Item.Episode != 19 {
		t.Errorf("p1 season/episode = %d/%d, want 2/19", p1.Item.Season, p1.Item.Episode)
	}
	if p1.Item.ShowTitle != "Simpsons" {
		t.Errorf("p1 showTitle = %q, want Simpsons", p1.Item.ShowTitle)
	}
	p2 := g.Programmes[1]
	if p2.Item.Season != 0 || p2.Item.Episode != 0 {
		t.Errorf("p2 'Game 7' falsely parsed as season/episode: %d/%d", p2.Item.Season, p2.Item.Episode)
	}
	p3 := g.Programmes[2]
	if p3.Item.Season != 3 || p3.Item.Episode != 4 {
		t.Errorf("p3 season/episode = %d/%d, want 3/4", p3.Item.Season, p3.Item.Episode)
	}
}

func TestXMLTVRoundTripAndHistoricalDates(t *testing.T) {
	guide := `<?xml version="1.0"?><tv>
<channel id="ch1"><display-name>Channel 1</display-name></channel>
<programme channel="ch1" start="20261019120000 +0000" stop="20261019130000 +0000">
  <title>Classic Movie</title>
  <date>19640510</date>
  <category>Cinema</category>
</programme>
<programme channel="ch1" start="20261019130000 +0000" stop="20261019140000 +0000">
  <title>Partial Date Show</title>
  <date>202405</date>
  <episode-num system="xmltv_ns">1 . .</episode-num>
</programme>
<programme channel="ch1" start="20261019140000 +0000" stop="20261019150000 +0000">
  <title>Unknown Season Episode</title>
  <date>1964</date>
  <episode-num system="xmltv_ns">. 4 .</episode-num>
</programme>
</tv>`
	g, err := ParseXMLTV(strings.NewReader(guide))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Programmes) != 3 {
		t.Fatalf("expected 3 programmes, got %d", len(g.Programmes))
	}

	// 1. Historical date check (negative Unix milliseconds)
	p1 := g.Programmes[0]
	if p1.Item.Date != "19640510" {
		t.Errorf("p1 Date = %q, want 19640510", p1.Item.Date)
	}
	if p1.Item.ReleasedMS >= 0 {
		t.Errorf("p1 ReleasedMS = %d, expected negative ms for 1964", p1.Item.ReleasedMS)
	}

	// 2. Partial date YYYYMM and partial episode "1 . ." (Season 2, unknown episode)
	p2 := g.Programmes[1]
	if p2.Item.Date != "202405" {
		t.Errorf("p2 Date = %q, want 202405", p2.Item.Date)
	}
	if p2.Item.Season != 2 || p2.Item.Episode != 0 {
		t.Errorf("p2 Season/Episode = %d/%d, want 2/0", p2.Item.Season, p2.Item.Episode)
	}

	// 3. Partial date YYYY and partial episode ". 4 ." (unknown season, Episode 5)
	p3 := g.Programmes[2]
	if p3.Item.Date != "1964" {
		t.Errorf("p3 Date = %q, want 1964", p3.Item.Date)
	}
	if p3.Item.Season != 0 || p3.Item.Episode != 5 {
		t.Errorf("p3 Season/Episode = %d/%d, want 0/5", p3.Item.Season, p3.Item.Episode)
	}

	// 4. Test Export -> Re-import Round Trip via writeXMLTV encoding logic
	buf := &bytes.Buffer{}
	writeXMLTV(buf, []Channel{{ID: "ch1", Name: "Channel 1"}}, g.Programmes)
	reimported, err := ParseXMLTV(buf)
	if err != nil {
		t.Fatalf("reimport failed: %v", err)
	}
	if len(reimported.Programmes) != 3 {
		t.Fatalf("reimported len = %d, want 3", len(reimported.Programmes))
	}

	rp1 := reimported.Programmes[0]
	if rp1.Item.Date != "19640510" || rp1.Item.ReleasedMS != p1.Item.ReleasedMS {
		t.Errorf("rp1 roundtrip failed: Date=%q ReleasedMS=%d", rp1.Item.Date, rp1.Item.ReleasedMS)
	}

	rp2 := reimported.Programmes[1]
	if rp2.Item.Date != "202405" || rp2.Item.Season != 2 || rp2.Item.Episode != 0 {
		t.Errorf("rp2 roundtrip failed: Date=%q Season=%d Episode=%d", rp2.Item.Date, rp2.Item.Season, rp2.Item.Episode)
	}

	rp3 := reimported.Programmes[2]
	if rp3.Item.Date != "1964" || rp3.Item.Season != 0 || rp3.Item.Episode != 5 {
		t.Errorf("rp3 roundtrip failed: Date=%q Season=%d Episode=%d", rp3.Item.Date, rp3.Item.Season, rp3.Item.Episode)
	}
}

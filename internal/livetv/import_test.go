package livetv

import (
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

func TestXMLTVSkipsBadTimes(t *testing.T) {
	doc := `<tv><channel id="a"/>
<programme start="2026-01-01T01:00:00Z" stop="20260101030000 +01:00" channel="a"><title>first</title></programme>
<programme start="invalid" channel="a"><title>bad start</title></programme>
<programme start="20260101020000+00:00" stop="invalid" channel="a"><title>bad stop</title></programme>
<programme start="202601010300 +00:00" stop="202601010400+00:00" channel="a"><title>last</title></programme></tv>`
	for _, filter := range []guideFilter{{}, {From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}} {
		g, dropped, err := parseXMLTV(strings.NewReader(doc), filter)
		if err != nil || !dropped || len(g.Programmes) != 2 || g.SkippedProgrammes != 2 {
			t.Fatalf("parse = %+v, skipped = %d, dropped = %v, err = %v", g.Programmes, g.SkippedProgrammes, dropped, err)
		}
		if g.Programmes[0].End.Hour() != 2 || g.Programmes[1].Item.Title != "last" {
			t.Fatalf("unexpected programmes: %+v", g.Programmes)
		}
	}
}

func TestXMLTVInfersStopBeyondWindow(t *testing.T) {
	doc := `<tv><channel id="a"/>
<programme start="20260101000000 +0000" channel="a"><title>one</title></programme>
<programme start="20260101010000 +0000" channel="a"><title>two</title></programme>
<programme start="20260101220000 +0000" channel="a"><title>out of order</title></programme>
<programme start="20260101200000 +0000" stop="20260101210000 +0000" channel="a"><title>far</title></programme></tv>`
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g, _, err := parseXMLTV(strings.NewReader(doc), guideFilter{From: day, Until: day.Add(12 * time.Hour)})
	if err != nil || len(g.Programmes) != 2 {
		t.Fatalf("programmes = %+v, err = %v", g.Programmes, err)
	}
	if got := g.Programmes[1].End; !got.Equal(day.Add(20 * time.Hour)) {
		t.Fatalf("inferred end = %v", got)
	}
}

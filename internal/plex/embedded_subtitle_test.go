package plex

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// extractTranscoder adds embedded-subtitle extraction to the fake.
type extractTranscoder struct {
	*fakeTranscoder
	out  string
	fail error
}

func (e *extractTranscoder) ExtractSubtitle(_ context.Context, _ string, _ int, _ string, w io.Writer) error {
	if e.fail != nil {
		return e.fail
	}
	_, err := io.WriteString(w, e.out)
	return err
}

const sampleVTT = "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nHello world\n"

// A subtitle picked from an embedded track during a transcode session is
// extracted and served on the title clock, instead of an empty container.
func TestEmbeddedSubtitleServedFromSession(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	const vtt = sampleVTT
	f.srv.d.Transcoder = &extractTranscoder{fakeTranscoder: f.trans, out: vtt}
	f.playback.info = probedInfo

	key := fightClub.Key.String()
	// Start a remux session so a transcode entry exists, then ask for the
	// embedded subtitle through the session endpoint.
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+key+"&directPlay=0&directStream=1&session=embsub1&protocol=hls",
		"X-Plex-Product", "Plex for Android (Mobile)", "X-Plex-Platform", "Android",
		"X-Plex-Client-Identifier", "client-1")
	if rec.Code != 200 {
		t.Fatalf("decision %d: %s", rec.Code, rec.Body.String())
	}
	// Embedded (French, index 3) subtitle id as rendered in metadata.
	f.get(t, "/library/metadata/"+key)
	embedded := core.StreamInfo{Index: 3, Type: "subtitle", Codec: "subrip", Language: "fr"}
	subID := slotHash(fightClub.Key, streamSlot(3, embedded))
	rec = f.get(t, fmt.Sprintf("/video/:/transcode/universal/subtitles?path=/library/metadata/%s&session=embsub1&subtitleStreamID=%d&format=vtt", key, subID),
		"X-Plex-Client-Identifier", "client-1")
	body := rec.Body.String()
	if body == "" || body == "<MediaContainer size=\"0\"/>" || !contains(body, "Hello world") {
		t.Fatalf("embedded subtitle not served: %q", body)
	}

	// Selecting the embedded subtitle in a remux decision must report it as
	// location="sidecar" with key="/library/streams/{id}" (since FFmpeg runs
	// with -sn) and serve extracted cues from /library/streams/{id}.
	decSub := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&directPlay=0&directStream=1&session=embsub2&protocol=hls&subtitleStreamID=%d", key, subID),
		"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android",
		"X-Plex-Client-Identifier", "client-1")
	if decSub.Code != 200 {
		t.Fatalf("decision with selected sub %d: %s", decSub.Code, decSub.Body.String())
	}
	streamKey := fmt.Sprintf("/library/streams/%d", subID)
	if !strings.Contains(decSub.Body.String(), `location="sidecar"`) || !strings.Contains(decSub.Body.String(), `key="`+streamKey+`"`) {
		t.Fatalf("selected embedded subtitle in remux decision missing sidecar key %s:\n%s", streamKey, decSub.Body.String())
	}
	streamRec := f.get(t, streamKey+"?format=vtt")
	if streamRec.Code != 200 || !contains(streamRec.Body.String(), "Hello world") {
		t.Fatalf("GET %s returned %d: %q", streamKey, streamRec.Code, streamRec.Body.String())
	}
}

// TestDirectPlayEmbeddedTextFollowsPMS is an embedded SRT or ASS selected
// for a direct play (#191). The track stays selected under every
// subtitles= mode and its row names the delivery the way PMS 1.42 does:
// Plex for Android and Android TV send subtitles=none and draw the track
// from the file (decision="ignore", no location); Plex Web sends
// subtitles=sidecar (location="sidecar-subs") and reads the cues as an ASS
// script from /subtitles/:/transcode/universal/start. A bitmap PGS is
// burned into the picture instead.
func TestDirectPlayEmbeddedTextFollowsPMS(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	streamer := &assStreamer{fakeTranscoder: f.trans, release: closedChan()}
	f.srv.d.Transcoder = streamer
	f.trans.decision = core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mkv"}
	f.playback.info = core.MediaInfo{
		Container: "mkv", Duration: 14 * time.Minute, BitrateKbps: 4000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 800, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
			{Index: 3, Type: "subtitle", Codec: "ass", Language: "fr"},
			{Index: 4, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "es"},
		},
	}
	key := fightClub.Key
	meta := f.get(t, "/library/metadata/"+key.String())
	tags := subtitleTags(meta.Body.String())
	srtID := subtitleChoice(tags, "eng", false)
	assID := subtitleChoice(tags, "fra", false)
	pgsID := ""
	for _, tag := range tags {
		if strings.Contains(tag, `codec="pgs"`) {
			pgsID = streamAttr(tag, "id")
		}
	}
	if srtID == "" || assID == "" || pgsID == "" {
		t.Fatalf("metadata subtitle ids srt=%q ass=%q pgs=%q tags=%q", srtID, assID, pgsID, tags)
	}
	web := []string{"X-Plex-Product", "Plex Web", "X-Plex-Platform", "Chrome", "X-Plex-Client-Identifier", "web-subs"}
	shield := []string{"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Client-Identifier", "shield-subs"}
	clients := []struct {
		name, protocol, subtitles string
		hdr                       []string
		want                      string
	}{
		{"web", "dash", "sidecar", web, `location="sidecar-subs"`},
		{"shield", "hls", "none", shield, `decision="ignore"`},
		{"shield-auto", "hls", "auto", shield, `location="direct"`},
	}
	for _, c := range clients {
		for _, id := range []string{srtID, assID} {
			putSubtitle(t, f, key.String(), id)
			session := c.name + "-" + id
			rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&protocol=%s&directPlay=1&directStream=1&hasMDE=1&session=%s&subtitles=%s", key, c.protocol, session, c.subtitles), c.hdr...)
			if rec.Code != 200 {
				t.Fatalf("%s decision %d: %s", c.name, rec.Code, rec.Body.String())
			}
			selected := selectedTag(subtitleTags(rec.Body.String()))
			if !strings.Contains(selected, `id="`+id+`"`) || !strings.Contains(selected, c.want) || strings.Contains(selected, `key="`) {
				t.Fatalf("%s subtitles=%s row: %s", c.name, c.subtitles, selected)
			}
			if c.subtitles == "none" && strings.Contains(selected, `location=`) {
				t.Fatalf("ignored track names a location: %s", selected)
			}
			if c.name != "web" {
				continue
			}
			got := f.get(t, fmt.Sprintf("/subtitles/:/transcode/universal/start?path=/library/metadata/%s&session=%s&directPlay=1&subtitles=sidecar&offset=125", key, session), c.hdr...)
			if got.Code != 200 || !strings.HasPrefix(got.Body.String(), "[Script Info]") || !strings.Contains(got.Body.String(), "Dialogue: 0,0:02:16.00") {
				t.Fatalf("web soft subtitle: %d %q", got.Code, got.Body.String())
			}
			if from := streamer.lastFrom(); from != 115*time.Second {
				t.Fatalf("soft subtitle read from %v, want the player position less the lead", from)
			}
		}
	}

	f.trans.decision = core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts", BurnSubtitles: true}
	putSubtitle(t, f, key.String(), pgsID)
	// The PGS was picked on the Apple TV; Plex Web is shown it selected
	// before it plays, so the track is burned (#313).
	f.get(t, "/library/metadata/"+key.String(), web...)
	rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&protocol=dash&directPlay=0&directStream=1&session=web-pgs", key), web...)
	selected := selectedTag(subtitleTags(rec.Body.String()))
	if !strings.Contains(selected, `decision="burn"`) || !strings.Contains(selected, `location="segments-video"`) {
		t.Fatalf("web PGS burn decision: %s\n%s", selected, rec.Body.String())
	}
}

// TestDirectStreamCarriesSelectedTextSubtitle is Plex for Android TV's
// own request (recorded Shield decision request: directPlay=1,
// directStream=1, hasMDE=1, subtitles=none, a Matroska HLS target) ending
// in a Direct Stream (#191). subtitles=none means the player draws the
// track; on a remux it reads it from the Matroska segments, so the run
// carries the selected SRT or ASS track and the row says so (PMS's copy
// row for a text track in Plex for Android's segments, subtitles_s1422).
// A conversion whose segments cannot carry text still delivers nothing.
func TestDirectStreamCarriesSelectedTextSubtitle(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	f.playback.info = core.MediaInfo{
		Container: "mkv", Duration: 14 * time.Minute, BitrateKbps: 4000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 800, Default: true},
			{Index: 1, Type: "audio", Codec: "truehd", Language: "en", Channels: 8, Default: true},
			{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
			{Index: 3, Type: "subtitle", Codec: "ass", Language: "fr"},
		},
	}
	// An on-demand (Usenet) release: Ether answers an MDE client's
	// decision with its own Direct Stream.
	id := fmt.Sprintf("ps-%d-%s-%d", ownerUser.ID, "shield-ds", fightClub.Key)
	f.playback.sessions[id] = &core.PlaySession{ID: id, UserID: ownerUser.ID, ClientID: "shield-ds", Key: fightClub.Key, Info: f.playback.info,
		StartedAt: fixedNow, Resolution: &core.Resolution{Release: core.Release{ID: "usenet-release"}}}
	key := fightClub.Key.String()
	tags := subtitleTags(f.get(t, "/library/metadata/"+key).Body.String())
	shield := []string{"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Client-Identifier", "shield-ds",
		"X-Plex-Client-Profile-Extra", "add-transcode-target(type=videoProfile&context=all&protocol=hls&container=mkv&videoCodec=h264,hevc&audioCodec=aac,ac3,eac3&subtitleCodec=ass,ssa,pgs,srt)"}
	for _, c := range []struct {
		lang, codec string
		index       int
	}{{"eng", "srt", 2}, {"fra", "ass", 3}} {
		putSubtitle(t, f, key, subtitleChoice(tags, c.lang, false))
		for _, container := range []string{transcode.ContainerMatroska, transcode.ContainerMPEGTS} {
			f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCodec: "eac3", Container: container}
			session := "ds-" + c.codec + "-" + container
			hdr := shield
			if container != transcode.ContainerMatroska {
				hdr = shield[:6] // no Matroska target
			}
			rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+key+"&protocol=hls&directPlay=1&directStream=1&hasMDE=1&subtitles=none&advancedSubtitles=text&session="+session, hdr...)
			if rec.Code != 200 {
				t.Fatalf("%s decision %d: %s", session, rec.Code, rec.Body.String())
			}
			row := selectedTag(subtitleTags(rec.Body.String()))
			got := f.trans.lastOpts.SubtitleStream
			if container != transcode.ContainerMatroska {
				// Segments that cannot carry text: nothing is delivered.
				if got != -1 || !strings.Contains(row, `decision="ignore"`) {
					t.Fatalf("%s: run subtitle %d, row %s", session, got, row)
				}
				continue
			}
			if got != c.index {
				t.Fatalf("%s: run carries subtitle %d, want %d", session, got, c.index)
			}
			if !strings.Contains(row, `decision="copy"`) || !strings.Contains(row, `location="segments-av"`) || !strings.Contains(row, `format="`+c.codec+`"`) || strings.Contains(row, `key="`) {
				t.Fatalf("%s row: %s", session, row)
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// A subtitle whose cues are not read yet answers at once and stays alive
// with blank lines, instead of going silent until the source has been read:
// players drop a silent subtitle download after a few seconds (#191).
// /library/streams serves a whole extracted track (SRT, WebVTT); Plex Web's
// ASS stream sends its cues as they are read.
func TestSlowEmbeddedSubtitleAnswersImmediately(t *testing.T) {
	quiet, alive := subtitleQuietWait, subtitleKeepAlive
	subtitleQuietWait, subtitleKeepAlive = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { subtitleQuietWait, subtitleKeepAlive = quiet, alive })
	for _, format := range []string{"srt", "vtt", "ass"} {
		t.Run(format, func(t *testing.T) {
			f, _ := newSubtitleFixture(t)
			slow := &assStreamer{fakeTranscoder: f.trans, release: make(chan struct{})}
			f.srv.d.Transcoder = slow
			f.trans.decision = core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mpegts"}
			f.playback.info = core.MediaInfo{
				Container: "mkv", Duration: 14 * time.Minute, BitrateKbps: 4000,
				Streams: []core.StreamInfo{
					{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 800, Default: true},
					{Index: 1, Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true},
					{Index: 2, Type: "subtitle", Codec: "subrip", Language: "en"},
				},
			}
			key := fightClub.Key
			client := []string{"X-Plex-Product", "Plex for Android (TV)", "X-Plex-Platform", "Android", "X-Plex-Client-Identifier", "slow-subs"}
			srtID := subtitleChoice(subtitleTags(f.get(t, "/library/metadata/"+key.String()).Body.String()), "eng", false)
			putSubtitle(t, f, key.String(), srtID)
			rec := f.get(t, fmt.Sprintf("/video/:/transcode/universal/decision?path=/library/metadata/%s&protocol=hls&directPlay=0&directStream=1&session=slow-subs", key), client...)
			target := streamAttr(selectedTag(subtitleTags(rec.Body.String())), "key") + "?format=" + format
			if format == "ass" {
				target = fmt.Sprintf("/video/:/transcode/universal/subtitles?path=/library/metadata/%s&session=slow-subs&format=ass", key)
			} else if strings.HasPrefix(target, "?") {
				t.Fatalf("no sidecar key: %s", rec.Body.String())
			}
			out := &flushSignal{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.RemoteAddr = "192.168.1.50:51000"
			req.Header.Set("X-Plex-Token", ownerToken)
			for i := 0; i+1 < len(client); i += 2 {
				req.Header.Set(client[i], client[i+1])
			}
			served := make(chan struct{})
			go func() { f.srv.ServeHTTP(out, req); close(served) }()
			select {
			case <-out.flushed: // answered while the track is still being read
			case <-time.After(5 * time.Second):
				t.Fatal("no response while the track is read")
			}
			close(slow.release)
			<-served
			body := out.Body.String()
			if out.Code != 200 || !strings.Contains(body, "Hello world") {
				t.Fatalf("%d %q", out.Code, body)
			}
			if format == "vtt" && (!strings.HasPrefix(body, "WEBVTT") || strings.Count(body, "WEBVTT") != 1) {
				t.Fatalf("vtt body must carry one header: %q", body)
			}
			if format == "ass" && (strings.TrimLeft(body, "\n") != assScript || out.Header().Get("Content-Type") != "text/x-ssa") {
				t.Fatalf("ass stream: %q %q", out.Header().Get("Content-Type"), body)
			}
		})
	}
}

// assScript is what the fake streamer writes: a header, then a cue.
const assScript = "[Script Info]\nScriptType: v4.00+\nPlayResX: 384\nPlayResY: 288\n\n[V4+ Styles]\n" +
	"Format: Name, Fontname, Fontsize\nStyle: Default,Arial,16\n\n[Events]\n" +
	"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
	"Dialogue: 0,0:02:16.00,0:02:20.00,Default,,0,0,0,,Hello world\n"

// assStreamer holds every read until release is closed, like FFmpeg
// reading a remote release, then answers sampleVTT (whole-track
// extraction) or assScript (StreamSubtitle).
type assStreamer struct {
	*fakeTranscoder
	release chan struct{}
	mu      sync.Mutex
	from    time.Duration
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func (e *assStreamer) lastFrom() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.from
}

func (e *assStreamer) wait(ctx context.Context) error {
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *assStreamer) ExtractSubtitle(ctx context.Context, _ string, _ int, _ string, w io.Writer) error {
	if err := e.wait(ctx); err != nil {
		return err
	}
	_, err := io.WriteString(w, sampleVTT)
	return err
}

func (e *assStreamer) StreamSubtitle(ctx context.Context, _ string, _ int, _ string, from time.Duration, w io.Writer) error {
	e.mu.Lock()
	e.from = from
	e.mu.Unlock()
	if err := e.wait(ctx); err != nil {
		return err
	}
	_, err := io.WriteString(w, assScript)
	return err
}

func (e *assStreamer) CachedSubtitle(context.Context, int, string) ([]byte, bool) { return nil, false }

// flushSignal reports the first Flush, the moment a client sees bytes.
type flushSignal struct {
	*httptest.ResponseRecorder
	once    sync.Once
	flushed chan struct{}
}

func (f *flushSignal) Flush() {
	f.ResponseRecorder.Flush()
	f.once.Do(func() { close(f.flushed) })
}

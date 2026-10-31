package plex

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// fakeVariantResolver serves a fixed variant list per title.
type fakeVariantResolver struct {
	byKey map[core.RatingKey][]core.ReleaseVariant
	calls int
	// lastMax is the cap the server asked for on the latest call.
	lastMax int
}

func (r *fakeVariantResolver) Variants(_ context.Context, req core.ResolveRequest, max int) ([]core.ReleaseVariant, error) {
	r.calls++
	r.lastMax = max
	vs := r.byKey[req.Title.Key]
	if max > 0 && len(vs) > max {
		vs = vs[:max]
	}
	return append([]core.ReleaseVariant(nil), vs...), nil
}

// fakeVariantPlayback adds PrepareVariant to the shared fake playback.
type fakeVariantPlayback struct {
	*fakePlayback
	variantCalls []string
	resumed      map[resumedFactKey]resumedFact
}

func (p *fakeVariantPlayback) PrepareVariant(_ context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps, releaseID string) (*core.PlaySession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.variantCalls = append(p.variantCalls, releaseID)
	id := fmt.Sprintf("psv-%d-%s-%d-%s", u.ID, caps.ClientID, k, releaseID)
	if ps, ok := p.sessions[id]; ok {
		return ps, nil
	}
	ps := &core.PlaySession{
		ID: id, UserID: u.ID, ClientID: caps.ClientID, Key: k, Info: p.info, StartedAt: fixedNow,
		Resolution: &core.Resolution{Title: k, Release: core.Release{ID: releaseID}},
	}
	p.sessions[id] = ps
	if u != nil && u.ID > 0 && releaseID != "" && !p.info.Estimated && len(p.info.Streams) > 0 {
		if p.resumed == nil {
			p.resumed = map[resumedFactKey]resumedFact{}
		}
		cp := p.info
		cp.Streams = append([]core.StreamInfo(nil), p.info.Streams...)
		p.resumed[resumedFactKey{u.ID, k}] = resumedFact{info: cp, release: releaseID}
	}
	return ps, nil
}

// resumedFact is the test double for playback.Manager.ResumedTracks: one
// probe per viewer and title. It does not apply profile, source, or block
// policy; the playback package tests those gates.
type resumedFactKey struct {
	user int64
	key  core.RatingKey
}

type resumedFact struct {
	info    core.MediaInfo
	release string
}

func (p *fakeVariantPlayback) ResumedTracks(_ context.Context, user *core.User, key core.RatingKey, _ core.ClientCaps) (core.MediaInfo, string, bool) {
	if user == nil || user.ID <= 0 {
		return core.MediaInfo{}, "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.resumed[resumedFactKey{user.ID, key}]
	if !ok || e.release == "" || e.info.Estimated || len(e.info.Streams) == 0 {
		return core.MediaInfo{}, "", false
	}
	return e.info, e.release, true
}

func mkVariant(id, label, edition string, auto bool, height int, vcodec, acodec string, dv bool, kbps int) core.ReleaseVariant {
	width := map[int]int{2160: 3840, 1080: 1920, 720: 1280}[height]
	video := core.StreamInfo{Index: 0, Type: "video", Codec: vcodec, Width: width, Height: height, FrameRate: 23.976, Default: true, DolbyVision: dv}
	if dv {
		video.ColorTransfer = "smpte2084"
		video.BitDepth = 10
	}
	info := core.MediaInfo{
		Container: "mkv", Duration: 139 * time.Minute, Size: 8_000_000_000, BitrateKbps: kbps, Estimated: true,
		Streams: []core.StreamInfo{video, {Index: 1, Type: "audio", Codec: acodec, Language: "en", Channels: 6, SampleRate: 48000, Default: true}},
	}
	return core.ReleaseVariant{ID: id, Label: label, Edition: edition, Auto: auto, Release: core.Release{ID: id}, Info: info}
}

func testVariantLists() map[core.RatingKey][]core.ReleaseVariant {
	return map[core.RatingKey][]core.ReleaseVariant{
		fightClub.Key: {
			mkVariant("rel-auto", "Auto / Best for Device", "", true, 1080, "h264", "eac3", false, 7600),
			mkVariant("rel-4k", "4K Dolby Vision Remux", "", false, 2160, "hevc", "truehd", true, 45000),
			mkVariant("rel-compat", "720p Blu-ray (High Compatibility)", "", false, 720, "h264", "aac", false, 4000),
		},
		alien.Key: {
			mkVariant("rel-dc", "Auto / Best for Device", "Director's Cut", true, 1080, "h264", "ac3", false, 8000),
			mkVariant("rel-alien4k", "4K HDR10 Blu-ray", "", false, 2160, "hevc", "dtshd", false, 30000),
		},
	}
}

type variantFixture struct {
	srv      *Server
	resolver *fakeVariantResolver
	playback core.Playback
}

func newVariantFixture(t *testing.T, playback core.Playback) *variantFixture {
	t.Helper()
	cat := &fakeCatalog{
		titles:   map[core.RatingKey]core.Title{fightClub.Key: fightClub, alien.Key: alien},
		children: map[core.RatingKey][]core.RatingKey{},
		facets:   map[string][]core.FacetValue{},
	}
	vr := &fakeVariantResolver{byKey: testVariantLists()}
	srv := NewServer(Deps{
		Catalog:           cat,
		State:             newFakeState(),
		Auth:              &fakeAuth{users: map[string]core.User{ownerToken: ownerUser, friendToken: friendUser}},
		Playback:          playback,
		Transcoder:        newFakeTranscoder(core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: "mkv"}),
		Variants:          vr,
		DisableEnrichment: true,
		Config: func() config.Config {
			c := config.Config{}
			c.Server.Port = 32400
			c.Server.FriendlyName = "Ether Test"
			c.Server.MachineIdentifier = "0123456789abcdef0123456789abcdef01234567"
			c.Transcode.RemoteMaxBitrateKbps = 8000
			return c
		},
		Identity: Identity{Version: "1.43.4.10903-e5521bd8c", Platform: "Linux"},
		Now:      func() time.Time { return fixedNow },
	})
	t.Cleanup(func() { srv.Close() })
	mountTestBroker(srv, func() time.Time { return fixedNow })
	return &variantFixture{srv: srv, resolver: vr, playback: playback}
}

func newVariantPlaybackFixture(t *testing.T) (*variantFixture, *fakeVariantPlayback) {
	t.Helper()
	pb := &fakeVariantPlayback{fakePlayback: newFakePlayback(sampleInfo)}
	return newVariantFixture(t, pb), pb
}

func (f *variantFixture) get(t *testing.T, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "192.168.1.50:51000"
	req.Header.Set("X-Plex-Token", ownerToken)
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

var variantPartRE = regexp.MustCompile(`/library/parts/(9[0-9]{15})/[0-9]+/file\.mkv`)

func TestDetailRendersPlayVersionMedia(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if n := strings.Count(body, "<Media "); n != 3 {
		t.Fatalf("media count %d, want 3\n%s", n, body)
	}
	// Auto keeps the historical ids and part key.
	autoID := fightClub.Key.String()
	if !strings.Contains(body, `id="`+autoID+`"`) || !strings.Contains(body, "/library/parts/"+autoID+"/") {
		t.Errorf("auto ids missing\n%s", body)
	}
	// The picker labels.
	if !strings.Contains(body, `title="Auto / Best for Device"`) ||
		!strings.Contains(body, `title="4K Dolby Vision Remux"`) ||
		!strings.Contains(body, "High Compatibility") {
		t.Errorf("variant labels missing\n%s", body)
	}
	// Per-variant quality attributes.
	if !strings.Contains(body, `videoResolution="4k"`) || !strings.Contains(body, `videoCodec="hevc"`) ||
		!strings.Contains(body, `audioCodec="truehd"`) {
		t.Errorf("variant quality attrs missing\n%s", body)
	}
	// Variant part ids are compact JSON-safe integers in the 8e15 block.
	m := variantPartRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no variant part key in\n%s", body)
	}
	vid, _ := strconv.ParseInt(m[1], 10, 64)
	if vid < 8_000_000_000_000_000 || vid >= 1<<53 {
		t.Errorf("variant id %d out of safe range", vid)
	}
	// Only the Auto version carries the stream list (2 estimated streams).
	if n := strings.Count(body, "<Stream "); n != 2 {
		t.Errorf("stream count %d, want 2 (Auto only)", n)
	}
	// No edition on this item.
	if strings.Contains(body, "editionTitle") {
		t.Errorf("unexpected editionTitle\n%s", body)
	}
	// JSON carries the same versions and labels.
	rec = f.get(t, "/library/metadata/"+fightClub.Key.String(), jsonAccept...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"4K Dolby Vision Remux"`) {
		t.Errorf("json variant label missing: %d %s", rec.Code, rec.Body.String())
	}
	// The rendered order is remembered for mediaIndex.
	ids, ok := f.srv.renderedVariants.get(renderedVariantKey{userID: ownerUser.ID, clientID: "client-1", key: fightClub.Key}, fixedNow)
	if !ok || len(ids) != 3 || ids[0] != "rel-auto" || ids[1] != "rel-4k" || ids[2] != "rel-compat" {
		t.Errorf("rendered set %v ok=%v", ids, ok)
	}
}

func TestDetailEditionTitleFromAutoVariant(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	rec := f.get(t, "/library/metadata/"+alien.Key.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `editionTitle="Director&#39;s Cut"`) {
		t.Errorf("editionTitle missing\n%s", rec.Body.String())
	}
}

func TestListResponsesStaySingleMedia(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	rec := f.get(t, "/library/sections/1/all?type=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), "<Media "); n != 2 {
		t.Errorf("list media count %d, want 2 (one per movie)", n)
	}
	if f.resolver.calls != 0 {
		t.Errorf("list rendering consulted variants %d times", f.resolver.calls)
	}
}

func TestMediaIndexSelectsVariant(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	f.get(t, "/library/metadata/"+fightClub.Key.String()) // renders + remembers
	base := "/video/:/transcode/universal/decision?path=/library/metadata/" + fightClub.Key.String()

	rec := f.get(t, base+"&mediaIndex=1&session=v1")
	if rec.Code != http.StatusOK {
		t.Fatalf("decision status %d: %s", rec.Code, rec.Body.String())
	}
	if len(pb.variantCalls) != 1 || pb.variantCalls[0] != "rel-4k" {
		t.Fatalf("variant calls %v", pb.variantCalls)
	}
	if pb.prepared != 0 {
		t.Errorf("plain Prepare used %d times for a variant pick", pb.prepared)
	}

	// Auto stays Auto.
	rec = f.get(t, base+"&mediaIndex=0&session=v2")
	if rec.Code != http.StatusOK || pb.prepared != 1 || len(pb.variantCalls) != 1 {
		t.Fatalf("auto decision code=%d prepared=%d variantCalls=%v", rec.Code, pb.prepared, pb.variantCalls)
	}
	// Out-of-range index falls back to Auto instead of failing.
	rec = f.get(t, base+"&mediaIndex=9&session=v3")
	if rec.Code != http.StatusOK || len(pb.variantCalls) != 1 {
		t.Fatalf("oob decision code=%d variantCalls=%v", rec.Code, pb.variantCalls)
	}
}

func TestMediaIndexRecomputesWithoutRender(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	// No detail render first: the selection is resolved by recomputation.
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&mediaIndex=2&session=v9")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(pb.variantCalls) != 1 || pb.variantCalls[0] != "rel-compat" {
		t.Fatalf("variant calls %v", pb.variantCalls)
	}
}

func TestVariantPartDirectPlay(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	partKey := variantPartRE.FindString(rec.Body.String())
	if partKey == "" {
		t.Fatalf("no variant part key")
	}
	rec = f.get(t, partKey, "Range", "bytes=0-9")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("part status %d: %s", rec.Code, rec.Body.String())
	}
	if len(pb.variantCalls) != 1 || pb.variantCalls[0] != "rel-4k" {
		t.Fatalf("variant calls %v", pb.variantCalls)
	}
	// The Auto part keeps working and never reaches PrepareVariant.
	rec = f.get(t, "/library/parts/"+fightClub.Key.String()+"/1/file.mkv", "Range", "bytes=0-9")
	if rec.Code != http.StatusPartialContent && rec.Code != http.StatusOK {
		t.Fatalf("auto part status %d", rec.Code)
	}
	if len(pb.variantCalls) != 1 {
		t.Errorf("auto part triggered PrepareVariant: %v", pb.variantCalls)
	}
	// Unknown part ids are 404, not a title.
	rec = f.get(t, "/library/parts/8500000000000000/1/file.mkv")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown variant part status %d", rec.Code)
	}
}

func TestVariantFallsBackWithoutVariantPlayback(t *testing.T) {
	plain := newFakePlayback(sampleInfo)
	f := newVariantFixture(t, plain)
	f.get(t, "/library/metadata/"+fightClub.Key.String())
	rec := f.get(t, "/video/:/transcode/universal/decision?path=/library/metadata/"+fightClub.Key.String()+"&mediaIndex=1&session=v5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if plain.prepared != 1 {
		t.Errorf("fallback prepared=%d, want 1 (normal Prepare)", plain.prepared)
	}
}

func TestVariantIDRegistryStablePersisted(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r1 := newVariantIDRegistry(db)
	id := r1.idFor(fightClub.Key, "rel-4k")
	if id < 8_000_000_000_000_000 || id >= 1<<53 {
		t.Fatalf("id %d out of range", id)
	}
	if r1.idFor(fightClub.Key, "rel-4k") != id {
		t.Fatal("id not stable within registry")
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM variants_ids WHERE media_id=?`, id).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("persisted rows = %d (err %v), want 1", rows, err)
	}
	// A fresh registry over the same DB (restart) resolves the same id.
	r2 := newVariantIDRegistry(db)
	if got := r2.idFor(fightClub.Key, "rel-4k"); got != id {
		t.Fatalf("persisted id %d != %d", got, id)
	}
	ref, ok := r2.lookup(id)
	if !ok || ref.key != fightClub.Key || ref.releaseID != "rel-4k" {
		t.Fatalf("lookup %+v ok=%v", ref, ok)
	}
}

func TestVariantIDCollisionProbeAndPartTitle(t *testing.T) {
	r := newVariantIDRegistry(nil)
	h := variantHash(fightClub.Key, "rel-x")
	squatter := variantRef{key: alien.Key, releaseID: "other"}
	r.byID[h] = squatter
	r.byPair[squatter] = h
	got := r.idFor(fightClub.Key, "rel-x")
	if got == h {
		t.Fatal("collision not probed")
	}
	if ref, ok := r.lookup(got); !ok || ref.releaseID != "rel-x" || ref.key != fightClub.Key {
		t.Fatalf("probed lookup %+v ok=%v", ref, ok)
	}
	if ref, ok := r.lookup(h); !ok || ref != squatter {
		t.Fatalf("squatter displaced: %+v", ref)
	}

	srv := &Server{variantIDs: r}
	if k, rel, ok := srv.partTitle(int64(fightClub.Key)); !ok || k != fightClub.Key || rel != "" {
		t.Errorf("auto partTitle %v %q %v", k, rel, ok)
	}
	if k, rel, ok := srv.partTitle(got); !ok || k != fightClub.Key || rel != "rel-x" {
		t.Errorf("variant partTitle %v %q %v", k, rel, ok)
	}
	if _, _, ok := srv.partTitle(8_500_000_000_000_000); ok {
		t.Error("unregistered variant id resolved")
	}
	if _, _, ok := srv.partTitle(12345); ok {
		t.Error("garbage part id resolved")
	}
}

func assertVIDAAXMLIdentity(t *testing.T, body string) {
	t.Helper()
	var doc struct {
		Videos []struct {
			Media []struct {
				ID    string `xml:"id,attr"`
				Parts []struct {
					ID string `xml:"id,attr"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Videos) != 1 || len(doc.Videos[0].Media) != 1 {
		t.Fatalf("unexpected media: %s", body)
	}
	m := doc.Videos[0].Media[0]
	if m.ID != fightClub.Key.String() || len(m.Parts) != 1 || m.Parts[0].ID != fightClub.Key.String() {
		t.Fatalf("media/part identity: %s", body)
	}
}

func TestVIDAAMetadataStaysSingleMediaAfterVariantsResolve(t *testing.T) {
	f, _ := newVariantPlaybackFixture(t)
	variants := f.resolver.byKey[fightClub.Key]
	f.resolver.byKey[fightClub.Key] = nil
	path := "/library/metadata/" + fightClub.Key.String()
	headers := []string{"X-Plex-Product", "Plex for VIDAA"}
	cold := f.get(t, path, headers...)
	if cold.Code != http.StatusOK || strings.Count(cold.Body.String(), "<Media ") != 1 {
		t.Fatalf("cold metadata: %d %s", cold.Code, cold.Body.String())
	}
	assertVIDAAXMLIdentity(t, cold.Body.String())
	f.resolver.byKey[fightClub.Key] = variants
	warm := f.get(t, path, headers...)
	if warm.Code != http.StatusOK || strings.Count(warm.Body.String(), "<Media ") != 1 {
		t.Fatalf("warm VIDAA metadata should retain one media: %d %s", warm.Code, warm.Body.String())
	}
	assertVIDAAXMLIdentity(t, warm.Body.String())
	for _, product := range []string{"plex for vidaa", "  Plex for VIDAA  "} {
		r := f.get(t, path, "X-Plex-Product", product)
		assertVIDAAXMLIdentity(t, r.Body.String())
	}
	jr := f.get(t, path, "X-Plex-Product", "Plex for VIDAA", "Accept", "application/json")
	var j map[string]any
	if err := json.Unmarshal(jr.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	mc := j["MediaContainer"].(map[string]any)
	v := mc["Metadata"].([]any)[0].(map[string]any)
	media := v["Media"].([]any)
	if len(media) != 1 {
		t.Fatalf("JSON media: %s", jr.Body.String())
	}
	jm := media[0].(map[string]any)
	parts := jm["Part"].([]any)
	if jm["id"] != float64(fightClub.Key) || len(parts) != 1 || parts[0].(map[string]any)["id"] != float64(fightClub.Key) {
		t.Fatalf("JSON media/part identity: %s", jr.Body.String())
	}
	other := f.get(t, path, "X-Plex-Product", "Plex Web")
	if n := strings.Count(other.Body.String(), "<Media "); other.Code != http.StatusOK || n != 3 {
		t.Fatalf("other client should retain all variants: %d media=%d", other.Code, n)
	}
}

func TestVIDAAPlaybackMetadataLifecycle(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	path := "/library/metadata/" + fightClub.Key.String()
	headers := []string{"X-Plex-Product", "Plex for VIDAA"}
	check := func(stage string) {
		t.Helper()
		r := f.get(t, path, headers...)
		if r.Code != 200 {
			t.Fatalf("%s metadata: %d %s", stage, r.Code, r.Body.String())
		}
		assertVIDAAXMLIdentity(t, r.Body.String())
	}
	check("warm")
	q := httptest.NewRequest(http.MethodPost, "/playQueues?type=video&uri="+urlEsc(path), nil)
	q.Header.Set("X-Plex-Token", ownerToken)
	q.Header.Set("X-Plex-Client-Identifier", "client-1")
	q.Header.Set("X-Plex-Product", "Plex for VIDAA")
	qr := httptest.NewRecorder()
	f.srv.ServeHTTP(qr, q)
	if qr.Code != 200 {
		t.Fatalf("queue: %d %s", qr.Code, qr.Body.String())
	}
	assertVIDAAXMLIdentity(t, qr.Body.String())
	query := "path=" + urlEsc(path) + "&session=vidaa&mediaIndex=0&directPlay=0&protocol=hls"
	for _, endpoint := range []string{"decision?", "start.m3u8?"} {
		r := f.get(t, "/video/:/transcode/universal/"+endpoint+query, headers...)
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", endpoint, r.Code, r.Body.String())
		}
		if endpoint == "decision?" {
			assertVIDAAXMLIdentity(t, r.Body.String())
		}
	}
	if pb.prepared == 0 || len(pb.variantCalls) != 0 {
		t.Fatalf("auto preparation: prepared=%d variants=%v", pb.prepared, pb.variantCalls)
	}
	ps, ok := pb.SessionFor(ownerUser.ID, "client-1", fightClub.Key)
	if !ok || len(ps.Info.Streams) == 0 || ps.Info.Streams[0].Width != sampleInfo.Streams[0].Width || ps.Info.Streams[0].Height != sampleInfo.Streams[0].Height || ps.Info.Streams[0].Codec != sampleInfo.Streams[0].Codec {
		t.Fatalf("prepared source: %+v", ps)
	}
	check("active")
	r := f.get(t, "/video/:/transcode/universal/start.m3u8?"+query+"&videoBitrate=2000&videoResolution=1280x720", headers...)
	if r.Code != 200 {
		t.Fatalf("quality switch: %d %s", r.Code, r.Body.String())
	}
	if r = f.get(t, "/video/:/transcode/universal/stop?session=vidaa", headers...); r.Code != 200 {
		t.Fatalf("stop: %d", r.Code)
	}
	check("stopped")
	if r = f.get(t, path, "X-Plex-Token", "invalid", "X-Plex-Product", "Plex for VIDAA"); r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized metadata: %d", r.Code)
	}
}

// ETHE-5 C: same-quality alternatives are separate Play Versions. Auto stays
// the first Media, each alternative prepares its own release by mediaIndex or
// part id, and plain Play still prepares Auto.
func TestSameQualityAlternativesAreSeparateVersions(t *testing.T) {
	f, pb := newVariantPlaybackFixture(t)
	alt := func(id, label string) core.ReleaseVariant {
		return mkVariant(id, label, "", false, 1080, "h264", "eac3", false, 7000)
	}
	f.resolver.byKey[fightClub.Key] = []core.ReleaseVariant{
		mkVariant("rel-auto", "Auto / Best for Device", "", true, 1080, "h264", "eac3", false, 7600),
		alt("rel-alt1", "1080p WEB-DL · DD+ 5.1 · 2.3 GB · FLUX"),
		alt("rel-alt2", "1080p WEB-DL · AAC 2.0 · 1.6 GB · EDITH"),
		mkVariant("rel-4k", "4K Dolby Vision Remux", "", false, 2160, "hevc", "truehd", true, 45000),
		mkVariant("rel-compat", "720p Blu-ray (High Compatibility)", "", false, 720, "h264", "aac", false, 4000),
	}
	rec := f.get(t, "/library/metadata/"+fightClub.Key.String())
	body := rec.Body.String()
	if n := strings.Count(body, "<Media "); n != 5 {
		t.Fatalf("media count %d, want 5\n%s", n, body)
	}
	if f.resolver.lastMax != core.MaxVariants {
		t.Errorf("render asked for %d versions, want core.MaxVariants=%d", f.resolver.lastMax, core.MaxVariants)
	}
	auto, a1, a2 := strings.Index(body, `title="Auto / Best`), strings.Index(body, "FLUX"), strings.Index(body, "EDITH")
	if auto < 0 || !(auto < a1 && a1 < a2) {
		t.Errorf("labels out of order auto=%d alt1=%d alt2=%d", auto, a1, a2)
	}
	if !strings.Contains(body, `id="`+fightClub.Key.String()+`"`) {
		t.Errorf("auto keeps the rating-key id")
	}

	// A client that addresses the alternative's part key reaches the same release.
	parts := regexp.MustCompile(`/library/parts/(9[0-9]{15})/[0-9]+/file\.mkv`).FindAllString(body, -1)
	if len(parts) != 4 {
		t.Fatalf("variant part keys %v", parts)
	}
	before := len(pb.variantCalls)
	if rec := f.get(t, parts[0], "Range", "bytes=0-9"); rec.Code != http.StatusPartialContent {
		t.Fatalf("part status %d", rec.Code)
	}
	if pb.variantCalls[before] != "rel-alt1" {
		t.Errorf("first variant part prepared %v", pb.variantCalls)
	}

	base := "/video/:/transcode/universal/decision?path=/library/metadata/" + fightClub.Key.String()
	for i, want := range []string{"rel-alt1", "rel-alt2"} {
		before := len(pb.variantCalls)
		if rec := f.get(t, base+fmt.Sprintf("&mediaIndex=%d&session=alt%d", i+1, i)); rec.Code != http.StatusOK {
			t.Fatalf("decision %d status %d: %s", i+1, rec.Code, rec.Body.String())
		}
		if len(pb.variantCalls) != before+1 || pb.variantCalls[before] != want {
			t.Fatalf("mediaIndex %d prepared %v, want %s", i+1, pb.variantCalls, want)
		}
	}
	// Plain Play (no mediaIndex, or index 0) prepares Auto only.
	n := len(pb.variantCalls)
	f.get(t, base+"&session=plain")
	f.get(t, base+"&mediaIndex=0&session=plain0")
	if pb.prepared != 2 || len(pb.variantCalls) != n {
		t.Errorf("plain play prepared=%d variantCalls=%v", pb.prepared, pb.variantCalls)
	}
}

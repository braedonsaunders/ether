package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/archive/archivetest"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/par2/par2test"
	"github.com/braedonsaunders/ether/internal/segcache"
)

// par2Index builds a PAR2 index file describing files (real name -> data).
func par2Index(sliceSize int64, files ...par2test.File) []byte {
	b, _ := par2test.Index(sliceSize, files)
	return b
}

// obfName is a random-looking name the classifier cannot read.
func obfName(i int) string { return fmt.Sprintf("q%02dX9k3Pq9LmZr7Tb2Wc8Hn4Jd", i) }

func TestPar2NamesObfuscatedPlainVideoWithoutExtraProbes(t *testing.T) {
	f := newFakeFetcher()
	video := mkvBytes(41, 600_000)
	sample := mkvBytes(42, 60_000)
	srt := []byte(strings.Repeat("1\n00:00:01,000 --> 00:00:02,000\nhello\n\n", 500))
	nfo := []byte("Release notes")
	idx := par2Index(16_384,
		par2test.File{Name: "Real.Movie.2024.1080p.mkv", Data: video},
		par2test.File{Name: "Real.Movie.2024.1080p-sample.mkv", Data: sample},
		par2test.File{Name: "Real.Movie.2024.1080p.srt", Data: srt},
		par2test.File{Name: "Real.Movie.2024.1080p.nfo", Data: nfo},
	)
	doc := post(t, f, 100_000,
		postFile{name: obfName(1), data: video},
		postFile{name: obfName(2), data: sample},
		postFile{name: obfName(3), data: srt},
		postFile{name: obfName(4), data: nfo},
		postFile{name: "d41d8cd98f00b204e9800998ecf8427e.par2", data: idx},
		postFile{name: "d41d8cd98f00b204e9800998ecf8427e.vol00+01.par2", data: randBytes(9, 120_000)},
	)
	e := newTestEngine(t, f, nil, Options{})
	src := openSource(t, e, "par2plain", doc)
	if src.Name() != "Real.Movie.2024.1080p.mkv" || src.Size() != int64(len(video)) {
		t.Fatalf("name %q size %d", src.Name(), src.Size())
	}
	// Only the video's head and the index were fetched: no probe of the
	// sample, subtitle or nfo, and no recovery volume.
	for i := 2; i <= 4; i++ {
		if n := f.calls(postID(obfName(i), 1)); n != 0 {
			t.Errorf("file %d probed %d times", i, n)
		}
	}
	if n := f.calls(postID("d41d8cd98f00b204e9800998ecf8427e.vol00+01.par2", 1)); n != 0 {
		t.Errorf("recovery volume fetched %d times", n)
	}
	if f.totalCalls() != 2 {
		t.Errorf("Open fetched %d articles, want 2 (video head, par2 index)", f.totalCalls())
	}
	st := e.Stats()
	if st.Par2Loaded != 1 || st.Par2Named != 1 || st.Par2ProbesSaved != 3 || st.Par2Verified != 1 || st.Par2Mismatches != 0 {
		t.Fatalf("stats %+v", st)
	}
	checkRandomReads(t, src, video, 3, 40)

	// The same post without PAR2 probes every unknown file.
	f2 := newFakeFetcher()
	doc2 := post(t, f2, 100_000,
		postFile{name: obfName(1), data: video},
		postFile{name: obfName(2), data: sample},
		postFile{name: obfName(3), data: srt},
		postFile{name: obfName(4), data: nfo},
	)
	e2 := newTestEngine(t, f2, nil, Options{})
	openSource(t, e2, "noPar2", doc2)
	if f2.totalCalls() != 4 {
		t.Fatalf("without par2 Open fetched %d heads, want 4", f2.totalCalls())
	}
}

// shuffledRAR4 builds an obfuscated RAR4 set whose volumes carry no volume
// numbers, posted in a shuffled order without "[n/N]" subject counters, so
// content sniffing cannot order it.
func shuffledRAR4(t *testing.T, video []byte) (vols [][]byte, order []int) {
	t.Helper()
	vols = buildRAR(t, []archivetest.RARFile{{Name: "Real.Name.mkv", Data: video}},
		archivetest.RAROptions{Version: 4, VolumeSize: 300_000, OmitEndVolumeNumber: true})
	order = rand.New(rand.NewPCG(5, 5)).Perm(len(vols))
	// Make sure the first volume is not posted first.
	if order[0] == 0 {
		order[0], order[1] = order[1], order[0]
	}
	return vols, order
}

func postShuffled(t *testing.T, f *fakeFetcher, vols [][]byte, order []int, extra ...postFile) []byte {
	t.Helper()
	var files []postFile
	for _, k := range order {
		name := obfName(10 + k)
		files = append(files, postFile{name: name, data: vols[k], subject: fmt.Sprintf(`"%s" yEnc (1/%d)`, name, (len(vols[k])+49_999)/50_000)})
	}
	return post(t, f, 50_000, append(files, extra...)...)
}

func rarPar2Files(vols [][]byte) []par2test.File {
	var out []par2test.File
	for k, v := range vols {
		out = append(out, par2test.File{Name: fmt.Sprintf("Real.Name.part%d.rar", k+1), Data: v})
	}
	return out
}

func TestPar2OrdersObfuscatedRARSet(t *testing.T) {
	video := mkvBytes(43, 1_200_000)
	vols, order := shuffledRAR4(t, video)
	if len(vols) < 4 {
		t.Fatalf("only %d volumes", len(vols))
	}

	// Control: without PAR2 the shuffled, unnumbered set cannot be ordered.
	f0 := newFakeFetcher()
	doc0 := postShuffled(t, f0, vols, order)
	e0 := newTestEngine(t, f0, nil, Options{})
	if src, err := e0.Open(context.Background(), "nopar2", doc0); err == nil {
		got := make([]byte, len(video))
		n, _ := src.ReadAt(got, 0)
		_ = src.Close()
		if n == len(video) && bytes.Equal(got, video) {
			t.Fatal("control: probe-only ordering succeeded; the fixture does not exercise PAR2 ordering")
		}
	}

	idx := par2Index(65_536, rarPar2Files(vols)...)
	for _, tc := range []struct {
		name     string
		par2Name string
	}{
		{"named index", "Real.Name.par2"},
		{"obfuscated index", obfName(99)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFetcher()
			doc := postShuffled(t, f, vols, order, postFile{name: tc.par2Name, data: idx})
			e := newTestEngine(t, f, nil, Options{})
			src := openSource(t, e, "par2rar", doc)
			if src.Name() != "Real.Name.mkv" || src.Size() != int64(len(video)) {
				t.Fatalf("name %q size %d", src.Name(), src.Size())
			}
			checkRandomReads(t, src, video, 4, 60)
			st := e.Stats()
			if st.Par2Loaded != 1 || st.Par2Named != int64(len(vols)) || st.Par2Verified != 1 {
				t.Fatalf("stats %+v", st)
			}
		})
	}
}

func TestPar2UnusableFallsBackToProbing(t *testing.T) {
	video := mkvBytes(44, 1_200_000)
	vols := buildRAR(t, []archivetest.RARFile{{Name: "Real.Name.mkv", Data: video}}, archivetest.RAROptions{Version: 5, VolumeSize: 300_000})
	var files []postFile
	for k := range vols {
		name := obfName(20 + k)
		files = append(files, postFile{name: name, data: vols[k],
			subject: fmt.Sprintf(`[%d/%d] - "%s" yEnc (1/%d)`, k+1, len(vols), name, (len(vols[k])+49_999)/50_000)})
	}
	for _, tc := range []struct {
		name    string
		par2    []byte
		missing bool
	}{
		{"garbage", randBytes(7, 3000), false},
		{"truncated", par2Index(65_536, rarPar2Files(vols)...)[:100], false},
		{"missing", par2Index(65_536, rarPar2Files(vols)...), true},
		{"foreign", par2Index(4096, par2test.File{Name: "Other.mkv", Data: mkvBytes(1, 5000)}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFetcher()
			doc := post(t, f, 50_000, append(append([]postFile{}, files...), postFile{name: "Real.Name.par2", data: tc.par2})...)
			if tc.missing {
				f.setMissing(postID("Real.Name.par2", 1))
			}
			e := newTestEngine(t, f, nil, Options{})
			src := openSource(t, e, "fallback", doc)
			if src.Name() != "Real.Name.mkv" {
				t.Fatalf("name %q", src.Name())
			}
			checkRandomReads(t, src, video, 5, 30)
			st := e.Stats()
			if st.Par2Named != 0 || st.Par2Mismatches != 0 {
				t.Fatalf("stats %+v", st)
			}
			ok, err := e.Check(context.Background(), doc)
			if !ok || err != nil {
				t.Fatalf("Check = %v %v", ok, err)
			}
		})
	}
}

func TestPar2MismatchIsUnhealthy(t *testing.T) {
	video := mkvBytes(45, 400_000)
	wrong := bytes.Clone(video)
	wrong[1000] ^= 0x55 // what the poster hashed differs in the first 16 KiB
	f := newFakeFetcher()
	doc := post(t, f, 100_000,
		postFile{name: "Movie.2024.mkv", data: video},
		postFile{name: "Movie.2024.par2", data: par2Index(16_384, par2test.File{Name: "Movie.2024.mkv", Data: wrong})},
	)
	e := newTestEngine(t, f, nil, Options{HealthSamples: 4})
	ok, err := e.Check(context.Background(), doc)
	if ok || !errors.Is(err, ErrPar2Mismatch) || !errors.Is(err, core.ErrReleaseRejected) || !IsRejection(err) {
		t.Fatalf("Check = %v %v", ok, err)
	}
	if tr := e.HealthTimings(); !strings.Contains(tr.FailReason, "first 16 KiB MD5") {
		t.Fatalf("fail reason %q", tr.FailReason)
	}
	// Open of the same release is rejected too, without fetching anything.
	f.resetCounts()
	if _, err := e.Open(context.Background(), "m", doc); !errors.Is(err, ErrPar2Mismatch) {
		t.Fatalf("Open = %v", err)
	}
	if f.totalCalls() != 0 {
		t.Fatalf("Open fetched %d", f.totalCalls())
	}
	if st := e.Stats(); st.Par2Mismatches != 1 {
		t.Fatalf("stats %+v", st)
	}

	// The matching PAR2 passes and counts a verification.
	f2 := newFakeFetcher()
	good := post(t, f2, 100_000,
		postFile{name: "Movie.2024.mkv", data: video},
		postFile{name: "Movie.2024.par2", data: par2Index(16_384, par2test.File{Name: "Movie.2024.mkv", Data: video})},
	)
	e2 := newTestEngine(t, f2, nil, Options{HealthSamples: 4})
	if ok, err := e2.Check(context.Background(), good); !ok || err != nil {
		t.Fatalf("good Check = %v %v", ok, err)
	}
	if st := e2.Stats(); st.Par2Verified != 1 || st.Par2Loaded != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestPar2SliceMismatchAndLengthMismatch(t *testing.T) {
	video := mkvBytes(46, 400_000)
	for _, tc := range []struct {
		name   string
		hashed []byte
		want   string
	}{
		{"slice", func() []byte { b := bytes.Clone(video); b[50_000] ^= 1; return b }(), "slice 6 CRC32"},
		{"length", append(bytes.Clone(video), 0), "length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeFetcher()
			doc := post(t, f, 100_000,
				postFile{name: "Movie.2024.mkv", data: video},
				postFile{name: "Movie.2024.par2", data: par2Index(8192, par2test.File{Name: "Movie.2024.mkv", Data: tc.hashed})},
			)
			e := newTestEngine(t, f, nil, Options{HealthSamples: 4})
			ok, err := e.Check(context.Background(), doc)
			if ok || !errors.Is(err, ErrPar2Mismatch) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check = %v %v", ok, err)
			}
		})
	}
}

func TestPar2ObfuscatedWrongContentRejectedAtOpen(t *testing.T) {
	// The only file of the PAR2's length is named by length, then fails
	// its 16k hash: Open rejects it rather than serving wrong content.
	video := mkvBytes(47, 500_000)
	hashed := bytes.Clone(video)
	hashed[10] ^= 1
	f := newFakeFetcher()
	doc := post(t, f, 100_000,
		postFile{name: obfName(1), data: video},
		postFile{name: obfName(2), data: []byte("nfo")},
		postFile{name: "x.par2", data: par2Index(16_384, par2test.File{Name: "Real.mkv", Data: hashed})},
	)
	e := newTestEngine(t, f, nil, Options{})
	if _, err := e.Open(context.Background(), "w", doc); !errors.Is(err, ErrPar2Mismatch) || !IsRejection(err) {
		t.Fatalf("Open = %v", err)
	}
}

func TestPar2OversizeIndexSkipped(t *testing.T) {
	f := newFakeFetcher()
	video := mkvBytes(48, 300_000)
	doc := post(t, f, 400_000,
		postFile{name: "Movie.2024.mkv", data: video},
		postFile{name: "Movie.2024.par2", data: randBytes(3, 1_100_000)},
	)
	e := newTestEngine(t, f, nil, Options{HealthSamples: 4})
	if ok, err := e.Check(context.Background(), doc); !ok || err != nil {
		t.Fatalf("Check = %v %v", ok, err)
	}
	openSource(t, e, "big", doc)
	for i := 1; i <= 3; i++ {
		if n := f.calls(postID("Movie.2024.par2", i)); n != 0 {
			t.Fatalf("oversize par2 part %d fetched %d times", i, n)
		}
	}
	if st := e.Stats(); st.Par2Skipped != 1 || st.Par2Loaded != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestPar2CheckOfObfuscatedPost(t *testing.T) {
	// Check of an obfuscated post with PAR2: named, verified, healthy, and
	// the following Open needs nothing more.
	f := newFakeFetcher()
	video := mkvBytes(49, 600_000)
	doc := post(t, f, 100_000,
		postFile{name: obfName(1), data: video},
		postFile{name: obfName(2), data: randBytes(2, 20_000)},
		postFile{name: "x.par2", data: par2Index(16_384, par2test.File{Name: "Real.mkv", Data: video})},
	)
	e := newTestEngine(t, f, nil, Options{HealthSamples: 4})
	if ok, err := e.Check(context.Background(), doc); !ok || err != nil {
		t.Fatalf("Check = %v %v", ok, err)
	}
	if n := f.calls(postID(obfName(2), 1)); n != 0 {
		t.Fatalf("small file probed %d times", n)
	}
	f.resetCounts()
	src := openSource(t, e, "c", doc)
	if src.Name() != "Real.mkv" || f.totalCalls() != 0 {
		t.Fatalf("name %q, Open fetched %d", src.Name(), f.totalCalls())
	}
}

// A check that knows its release ID keeps the PAR2 index and the heads it
// read in the disk segment cache under that ID, so an Open on a fresh
// engine (empty hot buffer) with no provider at all still names and opens
// the release from the cache.
func TestCheckReleaseKeepsPar2AndHeadsInDiskCache(t *testing.T) {
	cache, err := segcache.Open(t.TempDir(), 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	f := newFakeFetcher()
	video := mkvBytes(49, 600_000)
	doc := post(t, f, 100_000,
		postFile{name: obfName(1), data: video},
		postFile{name: "x.par2", data: par2Index(16_384, par2test.File{Name: "Real.mkv", Data: video})},
	)
	e := newTestEngine(t, f, cache, Options{HealthSamples: 4})
	if ok, err := e.CheckRelease(context.Background(), "rel-par2", doc); !ok || err != nil {
		t.Fatalf("CheckRelease = %v %v", ok, err)
	}
	_ = e.Close() // cache writes are asynchronous; Close waits for them
	if used, _ := cache.Usage(); used == 0 {
		t.Fatal("check cached nothing on disk")
	}
	// Plain Check (no ID) must not have been what filled it: a fresh engine
	// over a dead fetcher can only succeed from the disk cache.
	dead := newFakeFetcher()
	dead.failAll = true
	e2 := newTestEngine(t, dead, cache, Options{HealthSamples: 4})
	src := openSource(t, e2, "rel-par2", doc)
	if src.Name() != "Real.mkv" {
		t.Fatalf("name %q from cache", src.Name())
	}
}

func TestObfuscatedUnrecognizableMultiPartFailsAfterLeadingPair(t *testing.T) {
	f := newFakeFetcher()
	var postFiles []postFile
	var p2Files []par2test.File
	for i := 10; i < 42; i++ {
		name := fmt.Sprintf("60d0b0d07a0f5c41ad028f7eb16667c4.%02d", i)
		data := randBytes(uint64(i), 100_000)
		postFiles = append(postFiles, postFile{name: name, data: data})
		p2Files = append(p2Files, par2test.File{Name: name, Data: data})
	}
	postFiles = append(postFiles, postFile{
		name: "60d0b0d07a0f5c41ad028f7eb16667c4.par2",
		data: par2Index(16_384, p2Files...),
	})
	doc := post(t, f, 50_000, postFiles...)
	e := newTestEngine(t, f, nil, Options{HealthSamples: 4})
	ok, err := e.Check(context.Background(), doc)
	if ok || err == nil {
		t.Fatalf("expected unrecognizable obfuscated post to fail Check, got ok=%v err=%v", ok, err)
	}
	// Only the leading 2 unknown files + the .par2 index (3 articles) should be
	// fetched instead of all 32 unknown files.
	if got := f.totalCalls(); got > 3 {
		t.Fatalf("Check fetched %d articles on unrecognizable post, want <= 3", got)
	}
}

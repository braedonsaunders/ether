package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

func box(k string, bs ...[]byte) []byte {
	b := bytes.Join(bs, nil)
	h := make([]byte, 8)
	binary.BigEndian.PutUint32(h, uint32(len(b)+8))
	copy(h[4:], k)
	return append(h, b...)
}
func words(vs ...uint32) []byte {
	b := make([]byte, len(vs)*4)
	for i, v := range vs {
		binary.BigEndian.PutUint32(b[i*4:], v)
	}
	return b
}
func indexedMP4(wide bool) []byte {
	ftyp := box("ftyp", []byte("isom0000"))
	mdat := box("mdat", make([]byte, 256))
	offsets := box("stco", words(0, 3, 24, 54, 124))
	if wide {
		offsets = box("co64", words(0, 3, 0, 24, 0, 54, 0, 124))
	}
	stbl := box("stbl", box("stsz", words(0, 0, 5, 10, 20, 30, 40, 50)), offsets, box("stsc", words(0, 2, 1, 2, 1, 3, 1, 1)), box("stts", words(0, 1, 5, 1000)), box("ctts", words(0, 3, 2, 0, 1, 100, 2, 0)), box("stss", words(0, 3, 1, 3, 5)))
	return bytes.Join([][]byte{ftyp, mdat, box("moov", box("trak", box("mdia", box("hdlr", append(words(0, 0), []byte("vide")...)), box("mdhd", words(0, 0, 0, 1000)), box("minf", stbl))))}, nil)
}
func TestMP4SyncSampleIndexSkipsPayload(t *testing.T) {
	for _, wide := range []bool{false, true} {
		b := indexedMP4(wide)
		r := &recordIndexReader{Reader: bytes.NewReader(b)}
		p := containerIndex(context.Background(), r, int64(len(b)))
		want := []IndexPoint{{24, 0}, {54, 2100 * time.Millisecond}, {124, 4 * time.Second}}
		if len(p) != len(want) {
			t.Fatalf("wide=%v: %+v", wide, p)
		}
		for i := range want {
			if p[i] != want[i] {
				t.Fatalf("wide=%v point %d: %+v", wide, i, p[i])
			}
		}
		for _, read := range r.reads {
			if read[0] >= 24 && read[0] < 280 {
				t.Fatalf("read media payload: %v", read)
			}
		}
		tm := TimeMap{Size: int64(len(b)), Duration: 5 * time.Second, Points: p}
		if tm.TimeAt(54) != 2100*time.Millisecond {
			t.Fatal("index not used for variable bitrate mapping")
		}
	}
}

type recordIndexReader struct {
	*bytes.Reader
	reads [][2]int64
}

func (r *recordIndexReader) ReadAt(b []byte, o int64) (int, error) {
	r.reads = append(r.reads, [2]int64{o, int64(len(b))})
	return r.Reader.ReadAt(b, o)
}
func elementBytes(id uint32, bs ...[]byte) []byte {
	b := bytes.Join(bs, nil)
	ids := words(id)
	for len(ids) > 1 && ids[0] == 0 {
		ids = ids[1:]
	}
	var size []byte
	if len(b) < 127 {
		size = []byte{byte(len(b)) | 0x80}
	} else {
		size = []byte{byte(len(b)>>8) | 0x40, byte(len(b))}
	}
	return bytes.Join([][]byte{ids, size, b}, nil)
}
func cue(stamp, pos uint32, track byte) []byte {
	return elementBytes(0xbb, elementBytes(0xb3, words(stamp)), elementBytes(0xb7, elementBytes(0xf7, []byte{track}), elementBytes(0xf1, words(pos))))
}
func indexedMKV() ([]byte, []IndexPoint) {
	info := elementBytes(0x1549a966, elementBytes(0x2ad7b1, words(1000000)))
	tracks := elementBytes(0x1654ae6b, elementBytes(0xae, elementBytes(0xd7, []byte{1}), elementBytes(0x83, []byte{2})), elementBytes(0xae, elementBytes(0xd7, []byte{2}), elementBytes(0x83, []byte{1})))
	seek := func(id, pos uint32) []byte {
		return elementBytes(0x4dbb, elementBytes(0x53ab, words(id)), elementBytes(0x53ac, words(pos)))
	}
	head := elementBytes(0x114d9b74, seek(0x1549a966, 0), seek(0x1654ae6b, 0), seek(0x1c53bb6b, 0))
	cluster := elementBytes(0x1f43b675, make([]byte, 200))
	p1 := uint32(len(head) + len(info) + len(tracks))
	p2 := p1 + uint32(len(cluster))
	cues := elementBytes(0x1c53bb6b, cue(0, p1, 2), cue(1000, p2, 1), cue(3000, p2, 2))
	head = elementBytes(0x114d9b74, seek(0x1549a966, uint32(len(head))), seek(0x1654ae6b, uint32(len(head)+len(info))), seek(0x1c53bb6b, p2+uint32(len(cluster))))
	payload := bytes.Join([][]byte{head, info, tracks, cluster, cluster, cues}, nil)
	header := elementBytes(0x1a45dfa3)
	b := append(header, elementBytes(0x18538067, payload)...)
	base := len(b) - len(payload)
	return b, []IndexPoint{{int64(base) + int64(p1), 0}, {int64(base) + int64(p2), 3 * time.Second}}
}
func TestMatroskaTailCuesSelectVideoTrack(t *testing.T) {
	b, want := indexedMKV()
	p := containerIndex(context.Background(), bytes.NewReader(b), int64(len(b)))
	if len(p) != len(want) {
		t.Fatalf("cues: %+v want %+v", p, want)
	}
	for i := range want {
		if p[i] != want[i] {
			t.Fatalf("point %d: %+v want %+v", i, p[i], want[i])
		}
	}
}
func TestContainerIndexMalformedAndCancelled(t *testing.T) {
	m, _ := indexedMKV()
	for _, b := range [][]byte{indexedMP4(false), m} {
		for n := 0; n < len(b); n++ {
			_ = containerIndex(context.Background(), bytes.NewReader(b[:n]), int64(n))
		}
	}
	b := indexedMP4(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &recordIndexReader{Reader: bytes.NewReader(b)}
	if len(containerIndex(ctx, r, int64(len(b)))) != 0 || len(r.reads) != 0 {
		t.Fatal("cancelled indexing performed IO")
	}
	// Reject oversized metadata before allocating/reading the advertised atom.
	h := words(indexBudget + 16)
	h = append(h, []byte("moov")...)
	if p := containerIndex(context.Background(), bytes.NewReader(h), indexBudget+16); len(p) != 0 {
		t.Fatal(p)
	}
}
func FuzzContainerIndex(f *testing.F) {
	m, _ := indexedMKV()
	f.Add(indexedMP4(false))
	f.Add(m)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > indexBudget {
			return
		}
		p := containerIndex(context.Background(), bytes.NewReader(b), int64(len(b)))
		for _, v := range p {
			if v.Offset < 0 || v.Offset >= int64(len(b)) {
				t.Fatal(v)
			}
		}
	})
}

var _ io.ReaderAt = (*recordIndexReader)(nil)

// indexedMKVNoSeekHead is indexedMKV without a SeekHead: the Cues sit
// after the clusters and must be found by walking segment elements.
func indexedMKVNoSeekHead() ([]byte, []IndexPoint) {
	info := elementBytes(0x1549a966, elementBytes(0x2ad7b1, words(1000000)))
	tracks := elementBytes(0x1654ae6b, elementBytes(0xae, elementBytes(0xd7, []byte{2}), elementBytes(0x83, []byte{1})))
	cluster := elementBytes(0x1f43b675, make([]byte, 200))
	p1 := uint32(len(info) + len(tracks))
	p2 := p1 + uint32(len(cluster))
	cues := elementBytes(0x1c53bb6b, cue(0, p1, 2), cue(3000, p2, 2))
	payload := bytes.Join([][]byte{info, tracks, cluster, cluster, cues}, nil)
	header := elementBytes(0x1a45dfa3)
	b := append(header, elementBytes(0x18538067, payload)...)
	base := len(b) - len(payload)
	return b, []IndexPoint{{int64(base) + int64(p1), 0}, {int64(base) + int64(p2), 3 * time.Second}}
}

func TestMatroskaTailCuesWithoutSeekHead(t *testing.T) {
	b, want := indexedMKVNoSeekHead()
	r := &recordIndexReader{Reader: bytes.NewReader(b)}
	p := containerIndex(context.Background(), r, int64(len(b)))
	if len(p) != len(want) {
		t.Fatalf("cues: %+v want %+v", p, want)
	}
	for i := range want {
		if p[i] != want[i] {
			t.Fatalf("point %d: %+v want %+v", i, p[i], want[i])
		}
	}
	// Cluster payloads are skipped, never read.
	for _, read := range r.reads {
		if read[1] > 64 && read[0] > 40 {
			// Only small element headers and the small metadata/cues
			// elements may be read; a 200-byte cluster read would show here.
			if read[1] >= 200 {
				t.Fatalf("read cluster payload: %v", read)
			}
		}
	}
}

// Fragmented MP4: empty sample tables in moov, timing in sidx.
func TestFragmentedMP4SidxIndex(t *testing.T) {
	ftyp := box("ftyp", []byte("iso60000"))
	moov := box("moov", box("mvhd", words(0, 0, 0, 1000)))
	// sidx v0: timescale 1000, EPT 0, first_offset 0, 3 refs of 100 bytes
	// and 2s each (subsegment sizes include their moof+mdat).
	sidx := box("sidx", words(0, 1, 1000, 0, 0, 0x00000003, 100, 2000, 0, 100, 2000, 0, 100, 2000, 0))
	media := make([]byte, 300)
	b := bytes.Join([][]byte{ftyp, moov, sidx, media}, nil)
	base := int64(len(ftyp) + len(moov) + len(sidx))
	p := containerIndex(context.Background(), bytes.NewReader(b), int64(len(b)))
	want := []IndexPoint{{base, 0}, {base + 100, 2 * time.Second}, {base + 200, 4 * time.Second}}
	if len(p) != len(want) {
		t.Fatalf("sidx points: %+v want %+v", p, want)
	}
	for i := range want {
		if p[i] != want[i] {
			t.Fatalf("point %d: %+v want %+v", i, p[i], want[i])
		}
	}
}

func TestMP4EditListShiftsPresentationTime(t *testing.T) {
	trak := box("edts", box("elst", words(0, 1, 10000, 400, 65536)))
	off, err := mp4EditOffset(trak, 1000, 1000)
	if err != nil || off != -400 {
		t.Fatalf("edit offset=%v err=%v", off, err)
	}
	trak = box("edts", box("elst", words(0, 2, 2000, 0xffffffff, 65536, 10000, 400, 65536)))
	off, err = mp4EditOffset(trak, 1000, 1000)
	if err != nil || off != 1600 {
		t.Fatalf("empty edit offset=%v err=%v", off, err)
	}
}

func TestMatroskaSeekHeadJumpsPastManyClustersWithoutScanningPayload(t *testing.T) {
	info := elementBytes(0x1549a966, elementBytes(0x2ad7b1, words(1000000)))
	tracks := elementBytes(0x1654ae6b, elementBytes(0xae, elementBytes(0xd7, []byte{1}), elementBytes(0x83, []byte{1})))
	var clusters [][]byte
	for i := 0; i < 200; i++ {
		clusters = append(clusters, elementBytes(0x1f43b675, elementBytes(0xe7, words(uint32(i*1000))), make([]byte, 64)))
	}
	clusterBytes := bytes.Join(clusters, nil)
	cuesOffset := uint32(len(info) + len(tracks) + len(clusterBytes) + 40)
	seekHead := elementBytes(0x114d9b74, elementBytes(0x4dbb, elementBytes(0x53ab, []byte{0x1c, 0x53, 0xbb, 0x6b}), elementBytes(0x53ac, words(cuesOffset))))
	cuesOffset = uint32(len(seekHead) + len(info) + len(tracks) + len(clusterBytes))
	seekHead = elementBytes(0x114d9b74, elementBytes(0x4dbb, elementBytes(0x53ab, []byte{0x1c, 0x53, 0xbb, 0x6b}), elementBytes(0x53ac, words(cuesOffset))))
	cues := elementBytes(0x1c53bb6b,
		cue(0, uint32(len(seekHead)+len(info)+len(tracks)), 1),
		cue(5000, uint32(len(seekHead)+len(info)+len(tracks)+500), 1),
	)
	segBody := bytes.Join([][]byte{seekHead, info, tracks, clusterBytes, cues}, nil)
	segHdr := []byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	ebml := elementBytes(0x1a45dfa3, elementBytes(0x4282, []byte("matroska")))
	file := bytes.Join([][]byte{ebml, segHdr, segBody}, nil)

	r := &recordIndexReader{Reader: bytes.NewReader(file)}
	pts := containerIndex(context.Background(), r, int64(len(file)))
	if len(pts) != 2 {
		t.Fatalf("expected 2 cue points, got %+v", pts)
	}
	if len(r.reads) > 12 {
		t.Fatalf("expected SeekHead jump to Cues in <=12 reads, got %d reads", len(r.reads))
	}
}

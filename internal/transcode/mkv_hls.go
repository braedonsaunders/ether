package transcode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// HLSContainer preserves explicit client targets separately from the formats
// its direct-play decoder accepts. Plex's enhanced player requests segmented
// Matroska; AVPlayer and browser HLS keep the standard TS/fMP4 path.
func HLSContainer(caps core.ClientCaps, videoCodec, audioCodec string) string {
	for _, c := range caps.HLSContainers {
		switch c {
		case ContainerMatroska:
			return ContainerMatroska
		case "mp4", ContainerFMP4:
			return ContainerFMP4
		}
	}
	return segmentContainer(videoCodec, audioCodec)
}

// Plex's enhanced player joins an initialization header and Matroska clusters.
// Repeating complete MKVs corrupts its virtual stream's Segment/SeekHead offsets.
// Use the stream segment muxer's separate header, continuous timestamps and
// cluster-only fragments; the playlist advertises the header with EXT-X-MAP.
func matroskaHLSMuxArgs(s runSpec, segDur time.Duration) []string {
	start, pattern := s.StartSeg, filepath.Join(s.Dir, "%05d.mkv")
	if s.ChunkDir != "" {
		segDur, start, pattern = s.chunkCut(), 0, filepath.Join(s.ChunkDir, "%05d.mkv")
	}
	args := []string{
		"-avoid_negative_ts", "disabled", "-max_muxing_queue_size", "4096",
		"-f", "ssegment", "-segment_format", "matroska", "-flags", "+global_header",
		"-individual_header_trailer", "0", "-reset_timestamps", "0",
		// PMS uses a positive timestamp origin for non-TS segmented HLS.
		// Preserve that origin across startup, seeks and release swaps.
		"-segment_header_filename", filepath.Join(s.Dir, s.InitName),
		"-segment_time", secs(segDur), "-segment_start_number", strconv.Itoa(start),
		"-segment_list_type", "m3u8", "-segment_list_size", "0",
		"-segment_list", filepath.Join(s.Dir, s.Playlist),
	}
	if s.jellyfinMatroskaChunks() {
		// Jellyfin's segment-muxer patch counts the first PTS twice in
		// its cut threshold. A negative first PTS also overflows its
		// saturation check, preventing every subsequent cut after a seek.
		// Keep the title clock positive before the outer muxer sees it.
		// The delta allows a cut at every keyframe even with the doubled
		// origin; collectChunks regroups those GOPs onto the actual plan.
		// Never cut within a GOP. Upstream FFmpeg uses the relative
		// clock below: it lacks Jellyfin's first-entry duration fix.
		args = append(args, "-output_ts_offset", secs(matroskaOrigin),
			"-segment_time_delta", secs(s.Start+matroskaOrigin))
	} else {
		args = append(args, "-segment_format_options", "output_ts_offset="+secs(s.Start+matroskaOrigin))
	}
	return append(args, pattern)
}

// matroskaOrigin is added to media time in segmented Matroska output.
const matroskaOrigin = 10 * time.Second

// Jellyfin also fixes the first playlist entry's duration for nonzero
// timestamps. Unpatched FFmpeg counts that entry from zero, so it must
// retain the run-relative outer clock instead.
func (s runSpec) jellyfinMatroskaChunks() bool {
	return s.ChunkDir != "" && s.Features.jellyfinSegmentClock
}

// Segmented Matroska chunks (chunks.go) are read and spliced here: the
// Clusters ffmpeg's stream segment muxer writes, millisecond
// TimestampScale. Its HEVC frames are length-prefixed NAL units (the hvcC
// CodecPrivate gives the length size), parameter sets in-band at
// keyframes.

// Cluster elements (the header's are in nativeprobe.go).
const (
	mkvTimestamp   = 0xE7
	mkvSimpleBlock = 0xA3
	mkvBlockGroup  = 0xA0
	mkvBlock       = 0xA1
	mkvRefBlock    = 0xFB
	mkvCRC32       = 0xBF
)

// mkvFrame is one block of a Matroska Cluster.
type mkvFrame struct {
	track   uint64
	at      time.Duration // presentation time, less matroskaOrigin
	key     bool
	cluster int64 // offset of its Cluster
	data    int64 // offset and size of its (unlaced) frame
	size    int64
}

// mkvChunkStart is the presentation time of the first block of track video
// (its keyframe) in a segmented Matroska chunk, less the output origin
// matroskaHLSMuxArgs adds. The first Cluster's Timestamp alone is not the
// chunk's start: matroskaenc opens a Cluster with the audio it held back
// for the keyframe, and with B-frames that audio precedes the keyframe by
// the reorder delay, which can exceed the slotting tolerance. Open-GOP
// leading pictures follow their keyframe there with earlier timestamps, so
// the first video block, not the earliest, is the cut. Without a video
// block (or track, video 0) it is the first Cluster's Timestamp.
func mkvChunkStart(path string, video uint64) (time.Duration, error) {
	start, clusters := time.Duration(0), 0
	err := mkvWalk(path, func(at time.Duration) bool {
		clusters++
		if clusters == 1 {
			start = at
		}
		return video != 0
	}, func(b mkvFrame) bool {
		if b.track == video {
			start = b.at
			return false
		}
		return true
	})
	if err == nil && clusters == 0 {
		err = fmt.Errorf("matroska: no Cluster Timestamp in %s", filepath.Base(path))
	}
	return start, err
}

// mkvLastVideo is the latest presentation time of a track video block in a
// segmented Matroska segment (the Clusters of its chunks); false when it
// has none.
func mkvLastVideo(path string, video uint64) (last time.Duration, ok bool, err error) {
	err = mkvWalk(path, nil, func(b mkvFrame) bool {
		if b.track == video && (!ok || b.at > last) {
			last, ok = b.at, true
		}
		return true
	})
	return last, ok, err
}

// mkvChunkEnd is the latest presentation time of any Cluster or block in a
// segmented Matroska chunk, less the output origin matroskaHLSMuxArgs adds;
// false when it has none. The segment muxer computes a chunk's playlist
// duration from the video reference stream and only cuts on video keyframes,
// so when audio outlasts the video stream at the end of a title the final
// chunk holds all of the trailing audio Clusters past the video duration.
func mkvChunkEnd(path string) (last time.Duration, ok bool, err error) {
	err = mkvWalk(path, func(at time.Duration) bool {
		if !ok || at > last {
			last, ok = at, true
		}
		return true
	}, func(b mkvFrame) bool {
		if !ok || b.at > last {
			last, ok = b.at, true
		}
		return true
	})
	return last, ok, err
}

// mkvWalk reads the Clusters a segmented Matroska file consists of, in
// order: cluster gets each Cluster's Timestamp and block each block;
// either stops the walk by returning false. Frames are skipped, not read.
func mkvWalk(path string, cluster func(at time.Duration) bool, block func(b mkvFrame) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	size := st.Size()
	for off := int64(0); off < size; {
		id, body, n, err := ebmlElement(f, off, size)
		if err != nil || id != mkvCluster {
			if off == 0 {
				return fmt.Errorf("matroska: %s does not start with a Cluster", name)
			}
			return fmt.Errorf("matroska: bad Cluster in %s", name)
		}
		start, end := off, size
		if n >= 0 && body+n < end {
			end = body + n
		}
		ms := int64(-1)
		for off = body; off < end; {
			cid, kb, ks, err := ebmlElement(f, off, end)
			if err != nil || ks < 0 {
				return fmt.Errorf("matroska: bad Cluster element in %s", name)
			}
			if cid == mkvCluster {
				break // the next Cluster ends an unknown-size one
			}
			switch cid {
			case mkvTimestamp:
				v, err := ebmlUint(f, kb, ks)
				if err != nil || v > 1<<53 {
					return fmt.Errorf("matroska: bad Cluster Timestamp in %s", name)
				}
				ms = int64(v)
				if cluster != nil && !cluster(mkvTime(ms, 0)) {
					return nil
				}
			case mkvSimpleBlock, mkvBlockGroup:
				if ms < 0 {
					// Blocks before the timestamp: not ffmpeg's layout.
					return fmt.Errorf("matroska: no Cluster Timestamp in %s", name)
				}
				if block == nil {
					break
				}
				b, ok := mkvReadBlock(f, cid, kb, ks)
				if !ok {
					break
				}
				b.at += mkvTime(ms, 0)
				b.cluster = start
				if !block(b) {
					return nil
				}
			}
			off = kb + ks
		}
		if ms < 0 {
			return fmt.Errorf("matroska: no Cluster Timestamp in %s", name)
		}
		if n >= 0 {
			off = end
		}
	}
	return nil
}

func mkvTime(ms, rel int64) time.Duration {
	return time.Duration(ms+rel)*time.Millisecond - matroskaOrigin
}

// mkvReadBlock reads a SimpleBlock or BlockGroup element's block header;
// at is relative to its Cluster.
func mkvReadBlock(r io.ReaderAt, id uint64, off, size int64) (mkvFrame, bool) {
	key := false
	if id == mkvBlockGroup {
		// A BlockGroup's Block is a keyframe unless it references another.
		key = true
		bb, bs := int64(-1), int64(0)
		for o := off; o < off+size; {
			gid, gb, gs, err := ebmlElement(r, o, off+size)
			if err != nil || gs < 0 {
				break
			}
			switch gid {
			case mkvBlock:
				bb, bs = gb, gs
			case mkvRefBlock:
				key = false
			}
			o = gb + gs
		}
		if bb < 0 {
			return mkvFrame{}, false
		}
		off, size = bb, bs
	}
	var b [12]byte
	n, _ := r.ReadAt(b[:max(0, min(size, int64(len(b))))], off)
	track, l := chunkVint(b[:n], false)
	if l == 0 || n < l+3 {
		return mkvFrame{}, false
	}
	if id == mkvSimpleBlock {
		key = b[l+2]&0x80 != 0
	}
	rel := int64(int16(binary.BigEndian.Uint16(b[l:])))
	hdr := int64(l + 3)
	return mkvFrame{track: track, at: time.Duration(rel) * time.Millisecond, key: key, data: off + hdr, size: size - hdr}, true
}

// ebmlElement reads the header of the EBML element at off (before end):
// its ID, the offset of its body and the body's size (-1 when unknown).
// The size may run past end: a truncated leaf, or a master element whose
// size is a placeholder the muxer fixes later.
func ebmlElement(r io.ReaderAt, off, end int64) (id uint64, body, size int64, err error) {
	var b [12]byte
	n, err := r.ReadAt(b[:max(0, min(end-off, int64(len(b))))], off)
	if n == 0 {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, 0, 0, err
	}
	id, il := chunkVint(b[:n], true)
	if il == 0 || il > 4 {
		return 0, 0, 0, errors.New("matroska: bad element ID")
	}
	sz, sl := chunkVint(b[il:n], false)
	if sl == 0 {
		return 0, 0, 0, errors.New("matroska: bad element size")
	}
	body, size = off+int64(il+sl), int64(sz)
	if sz == 1<<(7*sl)-1 {
		size = -1
	}
	return id, body, size, nil
}

// ebmlUint reads an unsigned integer element body.
func ebmlUint(r io.ReaderAt, off, size int64) (uint64, error) {
	if size <= 0 || size > 8 {
		return 0, errors.New("matroska: bad integer element")
	}
	b := make([]byte, size)
	if _, err := r.ReadAt(b, off); err != nil {
		return 0, err
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

// mkvVideoTrack is the TrackNumber of the first video track in a
// segmented Matroska header (0 when there is none) and, for HEVC, the size
// of its frames' NAL unit lengths from the hvcC CodecPrivate the run's
// muxer wrote: 0 for Annex B (and other codecs), -1 when unknown.
func mkvVideoTrack(path string) (track uint64, nalLen int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	var walk func(off, end int64, depth int) (uint64, int, error)
	walk = func(off, end int64, depth int) (uint64, int, error) {
		var number, kind uint64
		hevc, private, lengths := false, false, 0
		for off < end {
			id, body, size, err := ebmlElement(f, off, end)
			if err != nil {
				return 0, 0, err
			}
			if size < 0 || size > end-body {
				size = end - body
			}
			switch id {
			case mkvSegment, mkvTracks, mkvTrackEntry:
				if n, l, err := walk(body, body+size, depth+1); err != nil || n != 0 {
					return n, l, err
				}
			case mkvCodecID:
				id := make([]byte, min(size, 32))
				if _, err := f.ReadAt(id, body); err == nil {
					hevc = string(id) == "V_MPEGH/ISO/HEVC"
				}
			case mkvCodecPrivate:
				// hvcC, configurationVersion 1: lengthSizeMinusOne in
				// byte 21 (2 is reserved). Annex B extradata starts with
				// a start code. Anything else: unknown (-1).
				var cp [23]byte
				n, _ := f.ReadAt(cp[:min(size, int64(len(cp)))], body)
				switch {
				case n == len(cp) && cp[0] == 1 && cp[21]&3 != 2:
					lengths = int(cp[21]&3) + 1
				case bytes.HasPrefix(cp[:n], []byte{0, 0, 1}) || bytes.HasPrefix(cp[:n], []byte{0, 0, 0, 1}):
					lengths = 0
				default:
					lengths = -1
				}
				private = true
			case mkvTrackNumber, mkvTrackType:
				v, err := ebmlUint(f, body, size)
				if err != nil {
					return 0, 0, err
				}
				if id == mkvTrackNumber {
					number = v
				} else {
					kind = v
				}
			case mkvCluster: // past the header
				return 0, 0, nil
			}
			off = body + size
		}
		if depth == 3 && kind == 1 { // a video TrackEntry
			switch {
			case !hevc:
				lengths = 0
			case !private:
				lengths = -1
			}
			return number, lengths, nil
		}
		return 0, 0, nil
	}
	return walk(0, st.Size(), 0)
}

// HEVC NAL unit types.
const (
	hevcBLAWLP = 16
	hevcCRA    = 21
)

// nalHeaders is the offset of every NAL unit header in an access unit of
// nalLen-byte length-prefixed NAL units (1, 2 or 4) or Annex B (nalLen 0).
// Each unit must hold at least its two-byte header and the units must fill
// the access unit exactly: anything else is an error.
func nalHeaders(b []byte, nalLen int) ([]int, error) {
	var out []int
	switch nalLen {
	case 0:
		if !bytes.HasPrefix(b, []byte{0, 0, 1}) && !bytes.HasPrefix(b, []byte{0, 0, 0, 1}) {
			return nil, errors.New("hevc: access unit does not start with a start code")
		}
		for i := 0; i+2 < len(b); i++ {
			if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
				out = append(out, i+3)
				i += 2
			}
		}
		for k, at := range out {
			end := len(b)
			if k+1 < len(out) {
				end = out[k+1] - 3
			}
			if end-at < 2 {
				return nil, errors.New("hevc: truncated NAL unit")
			}
		}
		return out, nil
	case 1, 2, 4:
		for i := 0; i < len(b); {
			if len(b)-i < nalLen {
				return nil, errors.New("hevc: truncated NAL unit length")
			}
			n := 0
			for _, c := range b[i : i+nalLen] {
				n = n<<8 | int(c)
			}
			i += nalLen
			if n < 2 || n > len(b)-i {
				return nil, fmt.Errorf("hevc: NAL unit of %d bytes in %d", n, len(b)-i)
			}
			out = append(out, i)
			i += n
		}
		return out, nil
	}
	return nil, fmt.Errorf("hevc: NAL unit length size %d", nalLen)
}

// maxSpliceFrame bounds the keyframe mkvSpliceHEVC reads.
const maxSpliceFrame = 64 << 20

// mkvSpliceHEVC makes the first video keyframe of a Matroska chunk, when
// it is an open-GOP CRA picture, a broken link (BLA_W_LP) picture. A
// CRA's RASL leading pictures reference the GOP before it in its own
// stream; a player continuing from another stream's GOP holds other
// pictures there and would decode them against those (or fail on missing
// references). For a BLA picture decoders discard its RASL pictures and
// still decode its RADL ones (H.265 8.1.3, C.5.2.2), the splice behaviour
// the type exists for. The rewrite is the type field of every slice NAL
// unit header of the picture, in place: CRA and BLA slice segment headers
// have the same syntax (both IRAP, nal_unit_type 16..23), and nothing else
// in the stream depends on the type. nalLen is the frames' NAL unit
// length size (mkvVideoTrack). The whole access unit is parsed before
// anything is written; a malformed one, or one whose slices are not all
// CRA, is left unchanged with an error. The Cluster's CRC-32 is updated.
// It reports whether the chunk was changed.
func mkvSpliceHEVC(chunk string, video uint64, nalLen int) (bool, error) {
	var first mkvFrame
	found := false
	if err := mkvWalk(chunk, nil, func(b mkvFrame) bool {
		if b.track == video {
			first, found = b, true
		}
		return !found
	}); err != nil || !found || !first.key {
		return false, err
	}
	f, err := os.OpenFile(chunk, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if first.size <= 0 || first.size > maxSpliceFrame || first.data+first.size > st.Size() {
		return false, fmt.Errorf("matroska: keyframe of %d bytes at %d in a %d-byte chunk", first.size, first.data, st.Size())
	}
	au := make([]byte, first.size)
	if _, err := f.ReadAt(au, first.data); err != nil {
		return false, err
	}
	nals, err := nalHeaders(au, nalLen)
	if err != nil {
		return false, err
	}
	var cra []int
	slices := 0
	for _, at := range nals {
		switch t := int(au[at]>>1) & 0x3F; {
		case t == hevcCRA:
			cra = append(cra, at)
			slices++
		case t < 32: // another VCL NAL unit
			slices++
		}
	}
	if len(cra) == 0 {
		return false, nil // an IDR or closed GOP: nothing leads the keyframe
	}
	if len(cra) != slices {
		return false, fmt.Errorf("hevc: keyframe with %d CRA slices of %d", len(cra), slices)
	}
	for _, at := range cra {
		hdr := au[at]&0x81 | hevcBLAWLP<<1
		if _, err := f.WriteAt([]byte{hdr}, first.data+int64(at)); err != nil {
			return false, err
		}
	}
	return true, mkvFixCRC(f, first.cluster)
}

// mkvFixCRC recomputes the CRC-32 element a Cluster at off starts with
// (matroskaenc writes one): IEEE CRC-32 of the rest of its body,
// little-endian.
func mkvFixCRC(f *os.File, off int64) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	_, body, size, err := ebmlElement(f, off, st.Size())
	if err != nil || size < 0 {
		return err
	}
	cid, cb, cs, err := ebmlElement(f, body, body+size)
	if err != nil || cid != mkvCRC32 || cs != 4 {
		return err // no CRC to update
	}
	h := crc32.NewIEEE()
	if _, err := io.Copy(h, io.NewSectionReader(f, cb+4, body+size-cb-4)); err != nil {
		return err
	}
	var sum [4]byte
	binary.LittleEndian.PutUint32(sum[:], h.Sum32())
	_, err = f.WriteAt(sum[:], cb)
	return err
}

package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// TestHEVCHeaderContract exercises the three source representations through
// native probing, actual ffmpeg segmentation, ffprobe, and a decoder.
func TestHEVCHeaderContract(t *testing.T) {
	t.Run("representations", func(t *testing.T) {
		e := newRealEnv(t, WithoutHWDetection())
		e.gen(t, "source.mkv", 2, "-an", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "pools=1:frame-threads=1:keyint=24:bframes=0:repeat-headers=1")
		out, err := mediaproc.CommandContext(t.Context(), 30*time.Second, e.ffprobe, "-v", "error", "-show_streams", "-show_packets", "-show_data", "-of", "json", filepath.Join(e.dir, "source.mkv")).Output()
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Streams []struct {
				Extradata string `json:"extradata"`
			}
			Packets []struct {
				Data  string `json:"data"`
				PTS   int    `json:"pts"`
				Flags string `json:"flags"`
			}
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatal(err)
		}
		private := probeExtradata(parsed.Streams[0].Extradata)
		if !completeHEVCConfig(private) {
			t.Fatal("fixture lacks complete hvcC")
		}
		sets := hevcParameterNALs(private, 0)
		var annex []byte
		for _, typ := range []byte{32, 33, 34} {
			annex = append(annex, 0, 0, 0, 1)
			annex = append(annex, sets[typ]...)
		}
		empty := append([]byte(nil), private[:23]...)
		empty[22] = 0
		for _, tc := range []struct {
			name     string
			private  []byte
			annex    bool
			complete bool
		}{
			{"hvcc", private, false, true}, {"inband", empty, false, false}, {"annexb", annex, true, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var blocks [][]byte
				blocks = append(blocks, elU(0xe7, 0))
				for i, pkt := range parsed.Packets {
					data := probeExtradata(pkt.Data)
					if i == 0 { // Ensure the empty-configuration fixture carries all sets in-band.
						var prefix []byte
						for _, typ := range []byte{32, 33, 34} {
							n := sets[typ]
							prefix = binary.BigEndian.AppendUint32(prefix, uint32(len(n)))
							prefix = append(prefix, n...)
						}
						data = append(prefix, data...)
					}
					if tc.annex {
						var converted []byte
						for len(data) >= 4 {
							n := int(binary.BigEndian.Uint32(data))
							data = data[4:]
							if n > len(data) {
								t.Fatal("packet length")
							}
							converted = append(converted, 0, 0, 0, 1)
							converted = append(converted, data[:n]...)
							data = data[n:]
						}
						data = converted
					}
					flags := byte(0)
					if strings.Contains(pkt.Flags, "K") {
						flags = 0x80
					}
					block := []byte{0x81, byte(pkt.PTS >> 8), byte(pkt.PTS), flags}
					block = append(block, data...)
					blocks = append(blocks, el(mkvSimpleBlock, block))
				}
				b := el(mkvEBML, elU(0x4286, 1), elU(0x42f7, 1), elU(0x42f2, 4), elU(0x42f3, 8), elS(mkvDocType, "matroska"), elU(0x4287, 4), elU(0x4285, 2))
				b = append(b, el(mkvSegment, el(mkvInfo, elU(mkvTimecodeScl, 1000000), elF(mkvDuration, 2000)), el(mkvTracks, el(mkvTrackEntry, elU(mkvTrackNumber, 1), elU(mkvTrackUID, 1), elU(mkvTrackType, 1), elS(mkvCodecID, "V_MPEGH/ISO/HEVC"), el(mkvCodecPrivate, tc.private), elU(mkvDefaultDur, 41666667), el(mkvVideo, elU(mkvPixelWidth, 640), elU(mkvPixelHeight, 360)))), el(mkvCluster, blocks...))...)
				name := tc.name + ".mkv"
				if err := os.WriteFile(filepath.Join(e.dir, name), b, 0600); err != nil {
					t.Fatal(err)
				}
				info, err := ProbeSource(t.Context(), bytes.NewReader(b), int64(len(b)))
				if err != nil {
					t.Fatal(err)
				}
				if info.Video().HEVCParameterSets != tc.complete {
					t.Fatal("native filter evidence disagrees with source")
				}
				d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMatroska, VideoCopy: true, AudioStream: -1, SubtitleStream: -1}
				hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: tc.name, InputURL: e.url(name), Info: *info, Decision: d})
				if err != nil {
					t.Fatal(err)
				}
				joined := fetch(t, hs, InitSegment)
				joined = append(joined, fetch(t, hs, 0)...)
				path := filepath.Join(e.dir, tc.name+"-out.mkv")
				if err := os.WriteFile(path, joined, 0600); err != nil {
					t.Fatal(err)
				}
				streams := e.probeFile(t, path)
				if len(streams) != 1 || streams[0].CodecName != "hevc" {
					t.Fatalf("streams: %+v", streams)
				}
				if out, err := mediaproc.CommandContext(t.Context(), 30*time.Second, e.ffmpeg, "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput(); err != nil || len(out) > 0 {
					t.Fatalf("decode: %v: %s", err, out)
				}
				hs.Close()
			})
		}
	})
	t.Run("bounded-restarts", func(t *testing.T) {
		fr := newFakeRunner()
		fr.failStart = func([]string) (string, bool) {
			return "[hevc_mp4toannexb] No parameter sets in the extradata; Could not write header", true
		}
		failed := make(chan string, 16)
		m := newTestManager(t, fr, config.Transcode{HWAccel: "none"}, WithInputFailureHandler(func(_, input string, _ time.Duration, _ error) { failed <- input }))
		req := remuxRequest(t, "header-budget", time.Hour)
		req.Decision.Container = ContainerMatroska
		s := startSession(t, m, req)
		for i := 0; i < 7; i++ {
			select {
			case <-failed:
			case <-time.After(12 * time.Second):
				t.Fatal("failure callback missing")
			}
			for range 3 {
				s.mu.Lock()
				err := s.startRunForLocked(0, false, "client retry")
				s.mu.Unlock()
				if err == nil {
					t.Fatal("failed source restarted")
				}
			}
			if err := s.SwapInput(context.Background(), req.InputURL, req.Info, 0); err == nil {
				t.Fatal("failed input reused")
			}
			err := s.SwapInput(t.Context(), fmt.Sprintf("http://127.0.0.1:1/source/%d", i), req.Info, 0)
			if fr.startCount() == 8 {
				if err == nil {
					t.Fatal("session restart cap bypassed")
				}
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := fr.startCount(); n != 8 {
			t.Fatalf("started %d processes", n)
		}
	})
}

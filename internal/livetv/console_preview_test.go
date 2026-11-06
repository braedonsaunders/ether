package livetv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type consolePlayback struct{ lookDemoPlayback }

func (*consolePlayback) Sessions() []*core.PlaySession { return nil }

type consolePicker struct{}

func (consolePicker) Picks(ctx context.Context, u *core.User, ingredients map[string]float64, n int) ([]core.PersonalPick, error) {
	picks, err := (personalTestPicker{}).Picks(ctx, u, ingredients, n)
	for i := range picks {
		picks[i].Title.Runtime = time.Minute
	}
	return picks, err
}

type consoleUsers []core.User

func (u consoleUsers) Users(context.Context) ([]core.User, error) { return u, nil }

// Exercises the real admin mount, tune, shared switcher, manifest rewrite and
// segment route. Providers and credentials are local fixtures only.
func TestConsolePreviewBroadcast(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip(err)
	}
	fp, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	ctx := t.Context()
	clip := filepath.Join(dir, "programme.mp4")
	if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "60", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-g", "120", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ac", "2", clip).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	segment := filepath.Join(dir, "source.ts")
	if out, err := mediaproc.CommandContext(ctx, 2*time.Minute, ff, "-v", "error", "-y", "-i", clip, "-t", "4", "-c", "copy", "-f", "mpegts", segment).CombinedOutput(); err != nil {
		t.Fatalf("TS fixture: %v %s", err, out)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".ts") {
			w.Header().Set("Content-Type", "video/mp2t")
			http.ServeFile(w, r, segment)
			return
		}
		if r.URL.Path == "/outage" {
			http.Error(w, "offline", 503)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		n := time.Now().Unix() / 4
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:%d\n", n-5)
		for i := n - 5; i <= n; i++ {
			if i > n-5 {
				fmt.Fprintln(w, "#EXT-X-DISCONTINUITY")
			}
			fmt.Fprintf(w, "#EXTINF:4,\n%d.ts\n", i)
		}
	}))
	defer upstream.Close()
	manager, err := transcode.New(config.Transcode{FFmpegPath: ff, FFprobePath: fp, TempDir: filepath.Join(dir, "out"), HWAccel: "none"}, slog.New(slog.NewTextHandler(io.Discard, nil)), transcode.WithoutHWDetection())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	users := consoleUsers{{ID: 1, IsOwner: true, Title: "Owner"}, {ID: 2, Title: "Alice"}}
	s := testService(t, Options{Transcoder: manager, Playback: &consolePlayback{lookDemoPlayback{path: clip}}, Picker: consolePicker{}, Users: users, InputUser: func(context.Context) (*core.User, error) { return &users[0], nil }})
	channels := []Channel{}
	for i, name := range []string{"virtual", "iptv", "outage", "personal"} {
		c := testChannel()
		c.ID = name
		c.Name = "W14 " + name
		c.Number = strconv.Itoa(i + 1)
		c.Items = []Item{{Key: core.MovieKey(1), Title: "Local programme", DurationMS: 60000}}
		if name == "iptv" || name == "outage" {
			c.Kind = "iptv"
			c.Items = nil
			c.StreamURL = upstream.URL + "/index.m3u8"
		}
		if name == "outage" {
			c.StreamURL = upstream.URL + "/outage"
			c.Transcode = true
			c.Screens = &ChannelScreens{NoSignal: "bars"}
		}
		if name == "personal" {
			c.Items = nil
			c.Personal = &PersonalRecipe{Ingredients: []PersonalIngredient{{Kind: "picked", Share: 100, Enabled: true}}}
		}
		saved, err := s.SaveChannel(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		channels = append(channels, saved)
		if name == "personal" {
			for i := range users {
				if err := s.buildPersonal(ctx, saved, &users[i]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	router := admin.NewHandler(admin.Deps{Config: cfg, IsOwnerToken: func(_ context.Context, token string) bool { return token == "offline" }, Extensions: map[string]http.Handler{"livetv": s.Handler()}})
	server := httptest.NewServer(router)
	defer server.Close()
	request := func(method, path string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, server.URL+path, nil)
		req.Header.Set("Authorization", "Bearer offline")
		req.Header.Set("X-Ether-Request", "1")
		req.Header.Set("Content-Type", "application/json")
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, body
	}
	for _, c := range channels {
		t.Run(c.ID, func(t *testing.T) {
			for _, query := range func() []string {
				if c.Personal != nil {
					return []string{"", "?viewer=2"}
				}
				return []string{""}
			}() {
				res, body := request("POST", "/ether/api/livetv/channels/"+c.ID+"/play"+query)
				var session PreviewSession
				if err := json.Unmarshal(body, &session); err != nil || res.StatusCode != 200 {
					t.Fatalf("tune %d %s", res.StatusCode, body)
				}
				if c.Personal != nil && (session.Programme == nil || session.Programme.Item.Title == "") {
					t.Fatal("personal preview lost rundown")
				}
				// Cold-start timing is covered by the separate Chrome probe. This
				// router conformance check starts once the real encoder publishes.
				s.mu.Lock()
				ss := s.sessions[session.ID]
				s.mu.Unlock()
				if ss.broadcast != nil {
					index, err := ss.broadcast.output()
					if err != nil {
						t.Fatal(err)
					}
					wait, cancel := context.WithTimeout(ctx, 30*time.Second)
					_, err = index.Wait(wait, -1)
					cancel()
					if err != nil {
						t.Fatal(err)
					}
				}
				path := "/ether/api/livetv/" + session.Playlist
				res, body = request("GET", path)
				t.Logf("GET %s => %d %s\n%s", path, res.StatusCode, res.Header.Get("Content-Type"), body)
				if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "mpegurl") {
					t.Fatalf("not HLS: %d %s", res.StatusCode, body)
				}
				// Parse the live media structure, then fetch every advertised URI through
				// the actual owner mount (never directly from the upstream fixture).
				text := string(body)
				if !strings.HasPrefix(text, "#EXTM3U\n") || !strings.Contains(text, "#EXT-X-TARGETDURATION:") || !strings.Contains(text, "#EXTINF:") || strings.Contains(text, "#EXT-X-ENDLIST") {
					t.Fatalf("invalid live playlist: %s", body)
				}
				base, _ := url.Parse(path)
				count := 0
				parserLines := strings.Split(text, "\n")
				for i, line := range parserLines {
					if line == "" || strings.HasPrefix(line, "#") {
						continue
					}
					ref, err := url.Parse(line)
					if err != nil {
						t.Fatal(err)
					}
					uri := base.ResolveReference(ref)
					if uri.Host != "" || !strings.HasPrefix(uri.Path, "/ether/api/livetv/sessions/"+session.ID+"/") {
						t.Fatalf("segment escaped owner API: %s", uri)
					}
					res, data := request("GET", uri.String())
					if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "video/mp2t") {
						t.Fatalf("TS %s: %d %s", uri, res.StatusCode, data)
					}
					capture := filepath.Join(dir, fmt.Sprintf("segment-%d.ts", count))
					parserLines[i] = filepath.Base(capture)
					if err := os.WriteFile(capture, data, 0600); err != nil {
						t.Fatal(err)
					}
					out, err := mediaproc.CommandContext(ctx, 2*time.Minute, fp, "-v", "error", "-select_streams", "v:0", "-show_packets", "-show_entries", "packet=flags", "-of", "csv=p=0", capture).Output()
					if err != nil || !strings.HasPrefix(string(out), "K") {
						t.Fatalf("segment lacks keyframe start: %v %s", err, out)
					}
					count++
				}
				if count == 0 {
					t.Fatal("empty window")
				}
				// Independently demux the live playlist with the fetched owner
				// segments. Only URI locations change; tags and timing are exact.
				manifest := filepath.Join(dir, "parsed.m3u8")
				if err := os.WriteFile(manifest, []byte(strings.Join(parserLines, "\n")), 0600); err != nil {
					t.Fatal(err)
				}
				parseContext, parseCancel := context.WithTimeout(ctx, 20*time.Second)
				parsed, err := mediaproc.CommandContext(parseContext, 2*time.Minute, fp, "-v", "error", "-analyzeduration", "500000", "-probesize", "1000000", "-read_intervals", "%+0.1", "-show_entries", "stream=codec_name", "-of", "csv=p=0", manifest).CombinedOutput()
				parseCancel()
				if err != nil || !strings.Contains(string(parsed), "h264") || !strings.Contains(string(parsed), "aac") {
					t.Fatalf("HLS parser: %v %s", err, parsed)
				}
				res, _ = request("DELETE", "/ether/api/livetv/sessions/"+session.ID)
				if res.StatusCode != 200 {
					t.Fatal("stop", res.StatusCode)
				}
				s.broadcasts.invalidate(c.ID, false)
			}
		})
	}
	if os.Getenv("ETHER_PLAYER_DEMO") != "" {
		listener, err := net.Listen("tcp", "127.0.0.1:32556")
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		mux := http.NewServeMux()
		mux.HandleFunc("POST /w14/stop", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-stop:
			default:
				close(stop)
			}
			w.WriteHeader(204)
		})
		mux.Handle("/", router)
		demo := &http.Server{Handler: mux}
		go demo.Serve(listener)
		defer demo.Close()
		t.Log("demo on 32556")
		select {
		case <-stop:
		case <-time.After(30 * time.Minute):
		}
	}
}

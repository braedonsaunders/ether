package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTimelineLifecycleRequiresTrackedPlaybackAndStoppedEncoder(t *testing.T) {
	for _, leak := range []bool{false, true} {
		t.Run(fmt.Sprintf("encoder-leak-%v", leak), func(t *testing.T) {
			playing := false
			stopped := false
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Plex-Session-Identifier") != "real-session" {
					t.Errorf("unexpected session %q", r.Header.Get("X-Plex-Session-Identifier"))
				}
				switch r.URL.Path {
				case "/:/timeline":
					switch r.URL.Query().Get("state") {
					case "playing":
						playing = true
					case "stopped":
						stopped = true
					}
					fmt.Fprint(w, `{}`)
				case "/status/sessions":
					if playing && !stopped {
						fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"Session":{"id":"real-session"}}]}}`)
					} else {
						fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
					}
				case "/encoder/segment":
					if !leak && stopped {
						http.NotFound(w, r)
					} else {
						fmt.Fprint(w, "still running")
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer s.Close()
			captures, err := timelineLifecycle(context.Background(), s.Client(), s.URL, "owner", "web", "hls", "real-session", "/encoder/segment", http.Header{}, map[string]string{"movieKey": "123"})
			if leak {
				if err == nil {
					t.Fatal("leaked encoder passed lifecycle check")
				}
				return
			}
			if err != nil || len(captures) != 5 || !playing || !stopped {
				t.Fatalf("captures=%d playing=%v stopped=%v err=%v", len(captures), playing, stopped, err)
			}
		})
	}
}

func TestSeekReconnectFollowsActualPlaylistAndSegment(t *testing.T) {
	var sawSeek, sawReconnect bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video/:/transcode/universal/start.m3u8":
			sawSeek = r.URL.Query().Get("offset") == "3"
			fmt.Fprint(w, "#EXTM3U\nsession/real/base/index.m3u8\n")
		case "/video/:/transcode/universal/session/real/base/index.m3u8":
			sawReconnect = true
			fmt.Fprint(w, "#EXTM3U\nsegment.ts\n")
		case "/video/:/transcode/universal/session/real/base/segment.ts":
			fmt.Fprint(w, "media")
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	playlist := "/video/:/transcode/universal/session/real/base/index.m3u8"
	captures, segment, err := seekAndReconnect(context.Background(), s.Client(), s.URL, "owner", "web", "remux", "/video/:/transcode/universal/start.m3u8?session=real", playlist, http.Header{}, nil)
	if err != nil || len(captures) != 4 || !sawSeek || !sawReconnect || segment != "/video/:/transcode/universal/session/real/base/segment.ts" {
		t.Fatalf("captures=%d segment=%q seek=%v reconnect=%v err=%v", len(captures), segment, sawSeek, sawReconnect, err)
	}
}

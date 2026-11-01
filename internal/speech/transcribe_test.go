package speech

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEndpointJoinsV1(t *testing.T) {
	if got := Endpoint("http://192.168.68.85:1234"); got != "http://192.168.68.85:1234/v1/audio/transcriptions" {
		t.Fatal(got)
	}
	if got := Endpoint("http://host:1234/v1/"); got != "http://host:1234/v1/audio/transcriptions" {
		t.Fatal(got)
	}
}

func TestTranscribePostsWhisperRequest(t *testing.T) {
	var gotModel, gotFormat, gotLang, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		form, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		_ = params
		for {
			part, err := form.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "model":
				gotModel = string(b)
			case "response_format":
				gotFormat = string(b)
			case "language":
				gotLang = string(b)
			case "file":
				if len(b) < 44 {
					t.Errorf("file %d bytes", len(b))
				}
			}
		}
		io.WriteString(w, "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello\n")
	}))
	defer srv.Close()
	c := Client{BaseURL: srv.URL, APIKey: "secret", Model: "whisper-1", HTTP: srv.Client()}
	f, err := os.CreateTemp(t.TempDir(), "a.wav")
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 320) // 10 ms
	if err := writePCM(f, pcm, 16000); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := c.TranscribeFile(context.Background(), f.Name(), "en")
	if err != nil {
		t.Fatal(err)
	}
	if gotModel != "whisper-1" || gotFormat != "vtt" || gotLang != "en" || gotAuth != "Bearer secret" {
		t.Fatalf("model %q format %q lang %q auth %q", gotModel, gotFormat, gotLang, gotAuth)
	}
	if !strings.Contains(out, "Hello") {
		t.Fatal(out)
	}
}

func TestTranscribeReportsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"model not loaded"}}`)
	}))
	defer srv.Close()
	c := Client{BaseURL: srv.URL + "/v1", Model: "whisper-1", HTTP: srv.Client()}
	_, err := c.Transcribe(context.Background(), strings.NewReader("x"), "a.wav", "")
	if err == nil || !strings.Contains(err.Error(), "model not loaded") {
		t.Fatal(err)
	}
}

func TestSplitKeepsShortWavWhole(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "a.wav")
	if err != nil {
		t.Fatal(err)
	}
	if err := writePCM(f, make([]byte, 64), 16000); err != nil {
		t.Fatal(err)
	}
	f.Close()
	chunks, cleanup, err := splitWAV(f.Name(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(chunks) != 1 || chunks[0].path != f.Name() || chunks[0].offset != 0 {
		t.Fatalf("%+v", chunks)
	}
}

func TestCueOffsetStitchesChunks(t *testing.T) {
	start, end, ok := cueTimes("00:00:01.000 --> 00:00:02.000")
	if !ok || start != time.Second || end != 2*time.Second {
		t.Fatal(start, end, ok)
	}
	text := renderVTT([]cue{{start: start + time.Minute, end: end + time.Minute, text: "Later"}})
	if !strings.Contains(text, "00:01:01.000") {
		t.Fatal(text)
	}
}

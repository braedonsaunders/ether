// Package speech sends audio to an OpenAI-compatible transcription endpoint
// (Whisper and the servers that speak its API). Callers invoke it only after
// a viewer asks for a subtitle track.
package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client talks to /audio/transcriptions.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// Endpoint is the transcription URL for an owner-entered base.
// "http://host:1234" and "http://host:1234/v1" both work.
func Endpoint(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case base == "":
		return ""
	case strings.HasSuffix(base, "/audio/transcriptions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/audio/transcriptions"
	default:
		return base + "/v1/audio/transcriptions"
	}
}

func (c Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// Transcribe posts one audio file and returns the response body.
// response_format=vtt asks for timed cues. filename is the multipart name.
func (c Client) Transcribe(ctx context.Context, audio io.Reader, filename, language string) (string, error) {
	url := Endpoint(c.BaseURL)
	if url == "" || strings.TrimSpace(c.Model) == "" {
		return "", fmt.Errorf("speech-to-text endpoint and model are required")
	}
	if filename == "" {
		filename = "audio.wav"
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, audio); err != nil {
		return "", err
	}
	if err := w.WriteField("model", c.Model); err != nil {
		return "", err
	}
	if err := w.WriteField("response_format", "vtt"); err != nil {
		return "", err
	}
	if language = strings.TrimSpace(language); language != "" {
		if err := w.WriteField("language", language); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if key := strings.TrimSpace(c.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var api struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &api) == nil && api.Error.Message != "" {
			return "", fmt.Errorf("speech-to-text: %s", api.Error.Message)
		}
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("speech-to-text: %s", msg)
	}
	return strings.TrimSpace(string(raw)), nil
}

// TranscribeFile sends a wav, splitting it into 10-minute pieces when it is
// larger than one request should carry, and returns one WebVTT document.
func (c Client) TranscribeFile(ctx context.Context, wavPath, language string) (string, error) {
	chunks, cleanup, err := splitWAV(wavPath, 10*time.Minute)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if len(chunks) == 1 {
		f, err := os.Open(chunks[0].path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		return c.Transcribe(ctx, f, "audio.wav", language)
	}
	var cues []cue
	for _, ch := range chunks {
		f, err := os.Open(ch.path)
		if err != nil {
			return "", err
		}
		text, err := c.Transcribe(ctx, f, "audio.wav", language)
		f.Close()
		if err != nil {
			return "", err
		}
		part, err := parseCues(text)
		if err != nil {
			return "", err
		}
		for i := range part {
			part[i].start += ch.offset
			part[i].end += ch.offset
		}
		cues = append(cues, part...)
	}
	return renderVTT(cues), nil
}

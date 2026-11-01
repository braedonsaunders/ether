package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/langfeat"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/speech"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func (a *App) languageGates() langfeat.Gates {
	var g langfeat.Gates
	if a.watchlist != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st, err := a.watchlist.Settings(ctx)
		cancel()
		if err == nil {
			g.SubtitleTranslate = st.SubtitleTranslate
			g.SpeechToText = st.SpeechToText
			g.OverviewTranslate = st.OverviewTranslate
		}
	}
	if a.cfg != nil {
		c := a.cfg.Get().LLM
		_, base := c.Endpoint()
		g.TextReady = c.Enabled && strings.TrimSpace(c.Model) != "" && base != ""
		g.SpeechReady = strings.TrimSpace(c.SpeechBaseURL) != "" && strings.TrimSpace(c.SpeechModel) != ""
	}
	return g
}

func (a *App) languageChat(ctx context.Context, system, user string) (string, error) {
	if a.llm == nil {
		return "", errors.New("language model is off")
	}
	return a.llm.Chat(ctx, system, user)
}

func (a *App) languageTranscribe(ctx context.Context, wavPath, language string) (string, error) {
	if a.cfg == nil {
		return "", errors.New("speech-to-text is not set up")
	}
	c := a.cfg.Get().LLM
	client := speech.Client{BaseURL: c.SpeechBaseURL, APIKey: c.SpeechAPIKey, Model: c.SpeechModel}
	return client.TranscribeFile(ctx, wavPath, language)
}

func (a *App) languageSession(user *core.User, title *core.Title) *core.PlaySession {
	if a.playback == nil || user == nil || title == nil {
		return nil
	}
	for _, sess := range a.playback.Sessions() {
		if sess != nil && sess.UserID == user.ID && sess.Key == title.Key && sess.ID != "" {
			return sess
		}
	}
	return nil
}

func (a *App) languageAudio(ctx context.Context, user *core.User, title *core.Title) (string, func(), error) {
	sess := a.languageSession(user, title)
	if sess == nil || a.playback == nil || a.transcoder == nil {
		return "", nil, errors.New("play the title, then ask for subtitles")
	}
	input := a.playback.InputURL(sess.ID)
	if input == "" {
		return "", nil, errors.New("play the title, then ask for subtitles")
	}
	return speech.ExtractWAV(ctx, a.transcoder.FFmpegPath(), input)
}

func (a *App) languageSourceText(ctx context.Context, user *core.User, title *core.Title, except string) (string, string, error) {
	sess := a.languageSession(user, title)
	if sess == nil || a.playback == nil || a.transcoder == nil {
		return "", "", errors.New("play the title, then ask for subtitles")
	}
	input := a.playback.InputURL(sess.ID)
	if input == "" {
		return "", "", errors.New("play the title, then ask for subtitles")
	}
	for _, st := range sess.Info.Streams {
		if st.Type != "subtitle" || st.External || transcode.IsImageSubtitle(st.Codec) {
			continue
		}
		lang := langtag.Normalize(st.Language)
		if lang == "" {
			lang = "en"
		}
		if except != "" && lang == except {
			continue
		}
		var buf bytes.Buffer
		if err := a.transcoder.ExtractSubtitle(ctx, input, st.Index, st.Codec, &buf); err != nil || buf.Len() == 0 {
			continue
		}
		return lang, buf.String(), nil
	}
	return "", "", errors.New("this title has no subtitle track to translate yet")
}

func (a *App) translateOverview(w http.ResponseWriter, r *http.Request) {
	if a.lang == nil || a.catalog == nil {
		http.Error(w, "Translation is unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Key      string `json:"key"`
		Language string `json:"language"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	key, err := core.ParseRatingKey(body.Key)
	if err != nil {
		http.Error(w, "Unknown title", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	title, err := a.catalog.Get(ctx, key)
	if err != nil || title == nil {
		http.Error(w, "Unknown title", http.StatusNotFound)
		return
	}
	doc, err := a.lang.Queue(ctx, title, body.Language)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"summary": doc.Summary, "tagline": doc.Tagline})
}

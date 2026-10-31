package langfeat

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/subtitles"
)

func testService(t *testing.T, gates Gates) (*Service, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var chats, speeches atomic.Int32
	subs := subtitles.New(subtitles.Options{CacheDir: t.TempDir()})
	s := New(Options{
		Dir:   t.TempDir(),
		Gates: func() Gates { return gates },
		Translate: func(ctx context.Context, system, user string) (string, error) {
			chats.Add(1)
			if strings.Contains(system, "subtitle") {
				return `["Ils arrivent."]`, nil
			}
			return `{"overview":"Un insomniaque.","tagline":"Les eaux calmes."}`, nil
		},
		Transcribe: func(ctx context.Context, wavPath, language string) (string, error) {
			speeches.Add(1)
			if wavPath == "" || language != "fr" {
				t.Fatalf("transcribe %q %q", wavPath, language)
			}
			return "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nBonjour\n", nil
		},
		Subtitles: subs,
		Audio: func(ctx context.Context, user *core.User, title *core.Title) (string, func(), error) {
			return t.TempDir() + "/audio.wav", func() {}, nil
		},
	})
	return s, &chats, &speeches
}

func TestOffersDoNotCallAModel(t *testing.T) {
	s, chats, speeches := testService(t, Gates{SubtitleTranslate: true, SpeechToText: true, TextReady: true, SpeechReady: true})
	title := &core.Title{Key: core.MovieKey(7), Type: core.TypeMovie, Language: "en"}
	offers := s.Offers(title, "fra")
	if len(offers) != 2 || offers[0].ID != TranslateOfferID || offers[1].ID != TranscribeOfferID || offers[0].Language != "fr" {
		t.Fatalf("%+v", offers)
	}
	if chats.Load() != 0 || speeches.Load() != 0 {
		t.Fatal("search called a model")
	}
	if got := s.Offers(title, "fr"); len(got) != 2 {
		t.Fatal(got)
	}
	if chats.Load() != 0 || speeches.Load() != 0 {
		t.Fatal("a second search called a model")
	}
}

func TestSubtitleTranslationRunsOnceAndCaches(t *testing.T) {
	s, chats, _ := testService(t, Gates{SubtitleTranslate: true, TextReady: true})
	title := &core.Title{Key: core.MovieKey(8), Type: core.TypeMovie, Language: "en"}
	if _, err := s.o.Subtitles.StoreText(title, "en", "English", "1\n00:00:01,000 --> 00:00:02,000\nThey're coming.\n\n", 42); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fulfill(context.Background(), nil, title, TranslateOfferID); err == nil {
		t.Fatal("pick without a search should miss")
	}
	if chats.Load() != 0 {
		t.Fatal("a miss called the model")
	}
	s.Offers(title, "fr")
	st, err := s.Fulfill(context.Background(), &core.User{ID: 1}, title, TranslateOfferID)
	if err != nil || st.Language != "fr" || !strings.Contains(st.Title, "Translated") {
		t.Fatalf("%+v %v", st, err)
	}
	if chats.Load() != 1 {
		t.Fatalf("chats %d", chats.Load())
	}
	if _, err := s.Fulfill(context.Background(), &core.User{ID: 1}, title, TranslateOfferID); err != nil {
		t.Fatal(err)
	}
	if chats.Load() != 1 {
		t.Fatal("cached translation called the model again")
	}
	listed := s.o.Subtitles.ManualSidecars(title, nil)
	if len(listed) != 2 {
		t.Fatalf("cached tracks %+v", listed)
	}
}

func TestTranscriptRunsOnlyWhenPicked(t *testing.T) {
	s, _, speeches := testService(t, Gates{SpeechToText: true, SpeechReady: true})
	title := &core.Title{Key: core.MovieKey(9), Type: core.TypeMovie}
	s.Offers(title, "fr")
	if speeches.Load() != 0 {
		t.Fatal("search transcribed")
	}
	st, err := s.Fulfill(context.Background(), &core.User{ID: 1}, title, TranscribeOfferID)
	if err != nil || st.Language != "fr" {
		t.Fatal(err, st)
	}
	if speeches.Load() != 1 {
		t.Fatalf("speeches %d", speeches.Load())
	}
	if _, err := s.Fulfill(context.Background(), &core.User{ID: 1}, title, TranscribeOfferID); err != nil {
		t.Fatal(err)
	}
	if speeches.Load() != 1 {
		t.Fatal("cached transcript called speech-to-text again")
	}
}

func TestOverviewWaitsForDetailOrQueue(t *testing.T) {
	s, chats, _ := testService(t, Gates{OverviewTranslate: true, TextReady: true})
	title := &core.Title{Key: core.MovieKey(10), Type: core.TypeMovie, Language: "en", Summary: "An insomniac.", Tagline: "Quiet waters."}
	if summary, tagline, ok := s.Overview(context.Background(), title, "fr", false); ok || summary != "" || tagline != "" || chats.Load() != 0 {
		t.Fatalf("list row translated %q %q calls %d", summary, tagline, chats.Load())
	}
	summary, tagline, ok := s.Overview(context.Background(), title, "fr", true)
	if !ok || summary != "Un insomniaque." || tagline != "Les eaux calmes." || chats.Load() != 1 {
		t.Fatalf("%q %q %v calls %d", summary, tagline, ok, chats.Load())
	}
	if _, _, ok := s.Overview(context.Background(), title, "fr", false); !ok || chats.Load() != 1 {
		t.Fatal("cached overview missed or called the model")
	}
	if _, err := s.Queue(context.Background(), title, "fr"); err != nil || chats.Load() != 1 {
		t.Fatal(err, chats.Load())
	}
	if _, _, ok := s.Overview(context.Background(), title, "en", true); ok || chats.Load() != 1 {
		t.Fatal("translated into the title's own language")
	}
}

func TestFeaturesStayOffUntilEnabled(t *testing.T) {
	s, chats, speeches := testService(t, Gates{TextReady: true, SpeechReady: true})
	title := &core.Title{Key: core.MovieKey(11), Type: core.TypeMovie, Language: "en", Summary: "Hello"}
	if len(s.Offers(title, "de")) != 0 {
		t.Fatal("offers while switches are off")
	}
	if _, _, ok := s.Overview(context.Background(), title, "de", true); ok || chats.Load() != 0 || speeches.Load() != 0 {
		t.Fatal("overview while the switch is off")
	}
}

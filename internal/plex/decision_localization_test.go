package plex

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/plex/model"
)

func TestDecisionLocalizationPreservesSharedResponse(t *testing.T) {
	original := &model.MediaContainer{Size: 1, Extra: []model.Attr{model.A("directPlayDecisionCode", 3000), model.A("directPlayDecisionText", textDirectPlayDisable), model.A("transcodeDecisionText", textConversionOK), model.A("resourceSession", "player")}}
	r := httptest.NewRequest("GET", "/video/:/transcode/universal/decision", nil)
	r = r.WithContext(withRequest(r.Context(), &Request{Language: "fr-CA"}))
	got := localizeDecisionResponse(r, original).(*model.MediaContainer)
	if got.Extra[1].Value == textDirectPlayDisable || !strings.Contains(got.Extra[1].Value.(string), "désactivée") {
		t.Fatalf("French refusal %v", got.Extra)
	}
	if got.Extra[0].Value != 3000 || got.Extra[3].Value != "player" || original.Extra[1].Value != textDirectPlayDisable {
		t.Fatalf("shared response mutated: original=%v localized=%v", original.Extra, got.Extra)
	}
	original.Extra[1].Value = "Diagnostic 42: source-specific detail"
	got = localizeDecisionResponse(r, original).(*model.MediaContainer)
	if got.Extra[1].Value != original.Extra[1].Value {
		t.Fatal("unknown detail discarded")
	}
	r.URL.Path = "/library/metadata/1"
	if localizeDecisionResponse(r, original) != original {
		t.Fatal("non-decision response copied")
	}
}

func TestDecisionLanguageNegotiation(t *testing.T) {
	for _, tc := range []struct{ x, accept, want string }{
		{"fr-CA", "de", "fr"}, {"unknown", "fr", ""}, {"", "ca,fr;q=0.8", "fr"}, {"", "ca,nn", "es"}, {"", "nn,ca", "no"},
		{"", "zh-TW,fr;q=0.5", "fr"}, {"", "zh-Hant-HK,ca;q=0.8", "es"}, {"pt-PT", "", "pt-br"}, {"nb-NO", "", "no"},
		{"", "fr;q=0,de;q=0.8", "de"}, {"", "de;q=invalid,fr;q=0.5", "fr"}, {"en-GB", "fr", "en"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Language", tc.accept)
		r = r.WithContext(withRequest(r.Context(), &Request{Language: tc.x}))
		if got := decisionLanguage(r); got != tc.want {
			t.Errorf("X=%s Accept=%s: %s want %s", tc.x, tc.accept, got, tc.want)
		}
	}
}

func TestDecisionLocalizationHTTP(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	path := "/video/:/transcode/universal/decision?path=/library/metadata/1000000000000550&hasMDE=1"
	english := f.get(t, path, "Accept", "application/json")
	french := f.get(t, path, "Accept", "application/json", "X-Plex-Language", "fr")
	if english.Code != 200 || french.Code != 200 {
		t.Fatalf("status en=%d fr=%d", english.Code, french.Code)
	}
	en, fr := decodeJSON(t, english), decodeJSON(t, french)
	if en["mdeDecisionCode"] != fr["mdeDecisionCode"] || en["mdeDecisionText"] == fr["mdeDecisionText"] {
		t.Fatalf("English=%v French=%v", en, fr)
	}
}

func TestDecisionDiagnosticCatalogsPreserveTechnicalDetails(t *testing.T) {
	messages := []string{
		"App cannot direct play this item.",
		"App cannot direct play this item. Selected audio stream is not the first audio stream and app cannot select audio streams in direct play.",
		"App cannot direct play this item. Selected sidecar subtitle stream cannot be direct played.",
		"App cannot direct play this item. Selected embedded subtitle stream is not the first embedded subtitle stream.",
		"Cannot convert this item. No conversion profile found for protocol hls.",
		"File is unplayable. DoVi (Profile 5) color space is not supported.",
		"This app cannot play this item. The reason is: video.profile limitation applies: high 10 != baseline|main|high [list].",
		"App cannot direct play this item. No direct play video profile exists for protocol http, with container mkv, and video codec hevc.",
		"App cannot direct play this item. No direct play video profile exists for protocol http, with container mp4, video codec h264, and audio codec truehd.",
		"App cannot direct play this item. No direct play video profile exists for protocol http, with container mkv, using video codec hevc, audio codec aac, and subtitle codec ass.",
	}
	for lang := range decisionTranslations {
		phrases, ok := decisionDiagnosticPhrases[lang]
		if !ok {
			t.Fatalf("missing diagnostic catalog %s", lang)
		}
		for _, phrase := range phrases {
			if phrase == "" {
				t.Fatalf("empty phrase in %s", lang)
			}
		}
		for _, message := range messages {
			if got := localizeDecisionDiagnostic(lang, message); got == message {
				t.Errorf("%s did not translate %s", lang, message)
			}
		}
		limit := localizeDecisionDiagnostic(lang, messages[6])
		if !strings.Contains(limit, "video.profile: high 10 != baseline|main|high [list]") {
			t.Fatalf("lost limit facts: %s", limit)
		}
		profile := localizeDecisionDiagnostic(lang, messages[9])
		for _, fact := range []string{"protocol=http", "container=mkv", "video=hevc", "audio=aac", "subtitle=ass"} {
			if !strings.Contains(profile, fact) {
				t.Fatalf("lost %s in %s", fact, profile)
			}
		}
		unknown := "Diagnostic 42: retain this unknown source failure"
		if localizeDecisionDiagnostic(lang, unknown) != unknown {
			t.Fatalf("unknown diagnostic changed in%s", lang)
		}
	}
}

func TestDecisionDiagnosticLocalizationHTTP(t *testing.T) {
	for _, lang := range []string{"fr", "de"} {
		t.Run(lang, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			path := "/video/:/transcode/universal/decision?path=/library/metadata/" + fightClub.Key.String() + "&directPlay=1&protocol=hls"
			english := f.get(t, path, "Accept", "application/json", "X-Plex-Client-Profile-Name", "Android")
			localized := f.get(t, path, "Accept", "application/json", "X-Plex-Client-Profile-Name", "Android", "X-Plex-Language", lang)
			if english.Code != 200 || localized.Code != 200 {
				t.Fatalf("status%d/%d", english.Code, localized.Code)
			}
			en, translated := decodeJSON(t, english), decodeJSON(t, localized)
			if en["directPlayDecisionCode"] != translated["directPlayDecisionCode"] || !strings.Contains(translated["directPlayDecisionText"].(string), "container=mkv") || en["directPlayDecisionText"] == translated["directPlayDecisionText"] {
				t.Fatalf("en%v localized%v", en, translated)
			}
			missing := f.get(t, strings.Replace(path, "directPlay=1", "directPlay=0", 1), "Accept", "application/json", "X-Plex-Client-Profile-Name", "Generic", "X-Plex-Language", lang)
			mc := decodeJSON(t, missing)
			if missing.Code != 200 || mc["transcodeDecisionCode"] != float64(4005) || !strings.Contains(mc["transcodeDecisionText"].(string), decisionDiagnosticPhrases[lang][4]) {
				t.Fatalf("missing profile%v", mc)
			}
		})
	}
	originalText := "This app cannot play this item. The reason is: video.width limitation applies: 3840 > 1920."
	original := &model.MediaContainer{Extra: []model.Attr{model.A("directPlayDecisionText", originalText), model.A("generalDecisionCode", 3000)}}
	r := httptest.NewRequest("GET", "/video/:/transcode/universal/decision", nil)
	r = r.WithContext(withRequest(r.Context(), &Request{Language: "de"}))
	localized := localizeDecisionResponse(r, original).(*model.MediaContainer)
	if original.Extra[0].Value != originalText || localized.Extra[0].Value == originalText || localized.Extra[1].Value != 3000 {
		t.Fatalf("mutation or lost code: original%v translated%v", original, localized)
	}
}

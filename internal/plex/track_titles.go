package plex

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

func trueDisposition(v *bool) *bool {
	if v != nil && *v {
		return model.Ptr(true)
	}
	return nil
}

// These native display names retain the capitalization used on the wire.
var nativeTrackNames = map[string]string{
	"uk": "Українська", "zxx": "No linguistic content", "nn": "norsk nynorsk", "no": "norsk",
	"fr": "Français", "es": "Español", "pt": "Português", "it": "Italiano",
	"ru": "Русский", "fi": "Suomi", "sv": "Svenska", "da": "Dansk",
	"hr": "Hrvatski", "cs": "Čeština", "sk": "Slovenčina", "tr": "Türkçe",
	"id": "Indonesia", "ms": "Melayu", "nl": "Nederlands", "la": "Latin",
	"fil": "Filipino", "cy": "Cymraeg", "dv": "Divehi", "nr": "South Ndebele",
	"rom": "Romany", "mul": "Multiple languages", "mdr": "Mandar", "enm": "English (Weeb)",
}

func nativeLanguage(tag, fallback string) string {
	if name, ok := nativeTrackNames[canonicalLanguagePrimary(tag)]; ok {
		return name
	}
	n := display.Self.Name(language.Make(tag))
	if n == "" {
		return fallback
	}
	return n
}
func terminologyCode(tag, fallback string) string {
	b, _ := language.Make(tag).Base()
	if n := b.ISO3(); n != "und" {
		return n
	}
	return fallback
}
func unknownLanguageCode(code string) string {
	if code == "und" {
		return ""
	}
	b, err := language.ParseBase(code)
	if err != nil {
		return ""
	}
	return b.ISO3()
}

func trackLanguage(code, locale string) string {
	if code == "" || code == "und" {
		return ""
	}
	if locale == "" {
		return languageName(code)
	}
	tag, err := language.Parse(code)
	if err != nil {
		return code
	}
	name := display.Languages(language.Make(locale)).Name(tag)
	if name == "" {
		return code
	}
	return name
}

// Semantic words in stream labels are protocol output, independent of console UI.
var trackWordTranslations = map[string][4]string{
	"en": {"Unknown", "Forced", "SDH", "External"},
	"af": {"Onbekend", "Gedwing", "SDH", "Ekstern"},
	"ar": {"غير معروف", "إجباري", "SDH", "خارجي"},
	"bg": {"Unknown", "Forced", "SDH", "External"},
	"el": {"Unknown", "Forced", "SDH", "External"},
	"et": {"Tundmatu", "Sunnitud", "SDH", "Väline"},
	"fa": {"Unknown", "Forced", "SDH", "External"},
	"he": {"לא ידוע", "כפוי", "SDH", "חיצוני"},
	"hr": {"Nepoznato", "Prisilno", "SDH", "Vanjsko"},
	"id": {"Tidak diketahui", "Dipaksa", "SDH", "Eksternal"},
	"is": {"Unknown", "Forced", "SDH", "External"},
	"lt": {"Nežinoma", "Priverstiniai", "SDH", "Išoriniai"},
	"lv": {"Nezināms", "Piespiedu", "SDH", "Ārējs"},
	"ms": {"Tidak diketahui", "Dipaksa", "SDH", "Luaran"},
	"my": {"Unknown", "Forced", "SDH", "External"},
	"nb": {"Ukjent", "Tvunget", "SDH", "Ekstern"},
	"nn": {"Ukjent", "Tvunget", "SDH", "Ekstern"},
	"ca": {"Desconocido", "Forzado", "SDH", "Externo"},
	"sk": {"Neznámy", "Vynútené", "SDH", "Externé"},
	"sr": {"Непознато", "Принудно", "SDH", "Спољно"},
	"th": {"Unknown", "Forced", "SDH", "External"},
	"vi": {"Unknown", "Forced", "SDH", "External"},
	"de": {"Unbekannt", "Erzwungen", "SDH", "Extern"},
	"fr": {"Inconnu", "Forcé", "SDH", "Externe"},
	"es": {"Desconocido", "Forzado", "SDH", "Externo"},
	"it": {"Sconosciuto", "Forzato", "SDH", "Esterno"},
	"pt": {"Desconhecido", "Forçado", "SDH", "Externo"},
	"nl": {"Onbekend", "Geforceerd", "SDH", "Extern"},
	"da": {"Ukendt", "Tvungen", "SDH", "Ekstern"},
	"sv": {"Okänt", "Tvingad", "SDH", "Extern"},
	"no": {"Ukjent", "Tvunget", "SDH", "Ekstern"},
	"fi": {"Tuntematon", "Pakotettu", "SDH", "Ulkoinen"},
	"pl": {"Nieznany", "Wymuszone", "SDH", "Zewnętrzne"},
	"cs": {"Neznámý", "Vynucené", "SDH", "Externí"},
	"ru": {"Неизвестный", "Принудительно", "SDH", "Внешний"},
	"uk": {"Невідома", "Примусово", "SDH", "Зовнішній"},
	"ja": {"不明", "強制", "SDH", "外部"},
	"ko": {"알 수 없음", "강제", "SDH", "외부"},
	"zh": {"未知", "强制", "SDH", "外部"},
	"tr": {"Unknown", "Forced", "SDH", "External"},
	"hu": {"Ismeretlen", "Kényszerített", "SDH", "Külső"},
	"ro": {"Necunoscut", "Forțat", "SDH", "Extern"},
}

func trackWords(locale string) [4]string {
	if traditionalChinese(locale) {
		return [4]string{"Unknown", "Forced", "SDH", "External"}
	}
	if w, ok := trackWordTranslations[primaryLang(locale)]; ok {
		return w
	}
	return [4]string{"Unknown", "Forced", "SDH", "External"}
}
func codecInPlainTitle(product string) bool {
	switch product {
	case "Plex Web", "Plex for iOS", "Plex for Apple TV", "Plex for Android (TV)", "Plex for Android (Mobile)", "Plex for LG", "Plex for Samsung", "Plex for Windows", "Plex for Mac", "Plex for Xbox", "Plex for PlayStation 4", "Plex for PlayStation 5", "Plex for Vizio", "Plex HTPC for Windows", "Plex HTPC for Mac", "Plex HTPC for Linux", "Plex for Linux", "Plex for Fetch TV", "Plex for Smart TVs", "Plex for VIDAA", "Plex for X1":
		return true
	}
	return false
}
func projectStreamTitles(out []model.Stream, src []int, info core.MediaInfo, pr *Request) {
	for i := range out {
		st := info.Streams[src[i]]
		out[i].Language = trackLanguage(st.Language, pr.Language)
		out[i].ExtendedDisplayTitle = composeTrackTitle(st, pr.Language, true)
		out[i].DisplayTitle = untitledTrack(st, pr.Language, st.Type == "audio")
		if st.Type != "audio" && codecInPlainTitle(pr.Product) {
			out[i].DisplayTitle = untitledTrack(st, pr.Language, true)
		}
	}
}
func titleTokens(s string) []string {
	return strings.FieldsFunc(asciiLower(s), func(r rune) bool { return strings.ContainsRune(" \t\n\r-/()[]+,|:;&", r) })
}
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, s)
}
func trackCodec(st core.StreamInfo) string {
	if st.Type == "audio" {
		return audioStreamCodecLabel(st)
	}
	if st.Codec == "eia_608" {
		return "Closed Captions"
	}
	return strings.ToUpper(plexCodec(st.Codec))
}
func trackParts(st core.StreamInfo, locale string) []string {
	words := trackWords(locale)
	lang := trackLanguage(st.Language, locale)
	if lang == "" {
		lang = words[0]
	}
	parts := []string{lang}
	if st.Type == "audio" {
		parts = append(parts, trackCodec(st))
		if ch := channelsLabel(st.Channels, st.ChannelLayout); ch != "" {
			parts = append(parts, ch)
		}
	} else if st.Type == "subtitle" {
		if st.Forced {
			parts = append(parts, words[1])
		}
		if st.HearingImpaired != nil && *st.HearingImpaired {
			parts = append(parts, words[2])
		}
		parts = append(parts, trackCodec(st))
		if st.External {
			suffix := st.ProviderTitle
			if suffix == "" {
				suffix = words[3]
			}
			parts = append(parts, suffix)
		}
	}
	return parts
}
func untitledTrack(st core.StreamInfo, locale string, withCodec bool) string {
	if st.Type == "video" {
		title := videoDisplayTitle(st)
		if withCodec {
			return title
		}
		short, _, _ := strings.Cut(title, " (")
		return short
	}
	p := trackParts(st, locale)
	if st.Type == "audio" {
		return p[0] + " (" + strings.Join(p[1:], " ") + ")"
	}
	split := len(p) - 1
	if st.External {
		split--
	}
	plain := strings.Join(p[:split], " ")
	if withCodec {
		plain += " (" + strings.Join(p[split:], " ") + ")"
	}
	return plain
}
func composeTrackTitle(st core.StreamInfo, locale string, extended bool) string {
	if !extended {
		return untitledTrack(st, locale, st.Type == "audio" || locale == "")
	}
	if st.Title == "" {
		return untitledTrack(st, locale, true)
	}
	parts := trackParts(st, locale)
	if st.Type == "video" {
		plain := videoDisplayTitle(st)
		a, b, _ := strings.Cut(plain, " (")
		parts = []string{a, strings.TrimSuffix(b, ")")}
	}
	tokens := titleTokens(st.Title)
	has := func(v string) bool {
		for _, token := range tokens {
			if token == asciiLower(v) {
				return true
			}
		}
		return false
	}
	var remaining []string
	for i, part := range parts {
		first := strings.Fields(part)
		carried := len(first) > 0 && has(first[0])
		if i == 0 && st.Type != "video" {
			stored := languageName(st.Language)
			if a, _, ok := strings.Cut(stored, " ("); ok {
				stored = a
			}
			carried = len(strings.Fields(stored)) == 1 && has(stored) || has(languageTag(st.Language))
		}
		switch part {
		case trackWords(locale)[1]:
			carried = carried || has("Signs") || has("Songs")
		case trackWords(locale)[2]:
			carried = carried || has("CC") || has("HI")
		case "Mono":
			carried = carried || has("1.0")
		case "Stereo":
			carried = carried || has("2.0")
		}
		if part == trackCodec(st) {
			raw := strings.NewReplacer("-", "", ".", "", "_", "").Replace(st.Codec)
			carried = carried || has(raw) || (st.Codec == "h264" && has("AVC"))
		}
		if strings.Contains(part, "HDR") || strings.Contains(part, "DoVi") {
			carried = carried || has("HDR") || has("Dolby")
		}
		if !carried {
			remaining = append(remaining, part)
		}
	}
	if len(remaining) == 0 {
		return st.Title
	}
	return st.Title + " (" + strings.Join(remaining, " ") + ")"
}

func canonicalLanguagePrimary(code string) string {
	b, err := language.ParseBase(code)
	if err == nil {
		return b.String()
	}
	return code
}

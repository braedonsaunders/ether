package plex

import "strings"

// Client-facing shelf labels are selected independently of metadata language.
func upNextTitle(language string) string {
	language = strings.ToLower(strings.Split(strings.ReplaceAll(language, "_", "-"), "-")[0])
	titles := map[string]string{
		"de": "Demnächst", "fr": "À suivre", "ja": "次に登場", "es": "A continuación", "it": "Prossimo", "pt": "A seguir", "nl": "Hierna", "sv": "Härnäst", "da": "Næste", "nb": "Neste", "fi": "Seuraavaksi", "pl": "Następnie", "cs": "Následuje", "hu": "Következik", "ro": "Urmează", "tr": "Sıradaki", "ru": "Далее", "uk": "Далі", "ko": "다음", "zh": "即将播放",
	}
	if title := titles[language]; title != "" {
		return title
	}
	return "Up Next"
}

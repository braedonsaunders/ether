package releaseparse

import "testing"

// Non-ASCII titles whose Unicode upper case changes byte length must never
// panic the parser (Xtream VOD names crashed the server in v0.3.10).
func TestParseNeverPanicsOnLengthChangingCase(t *testing.T) {
	for _, name := range []string{
		"ıııııııııııııııııııııııı.2019.1080p.WEB",
		"ıııııııııııııııı 2019 1080p",
		"ſſſſſſſſſſſſſſſſ.2020.MULTI.1080p",
		"ıııııııııııııııııııııııııııııı.S01E02.720p",
		"Straße der Sehnsucht ßßßß MULTI-SUBS DUAL 2160p",
	} {
		_ = Parse(name)
	}
}

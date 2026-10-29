package trackprefs

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// 2: negação explícita não é SDH; "(CC)"/" CC " são SDH/CC.
func TestSDHNegationAndCCMarkers(t *testing.T) {
	for _, title := range []string{
		"No SDH", "Not SDH", "non-SDH", "Non-SDH subs", "English (No SDH)",
		"Not hearing impaired", "No hearing impaired", "non-hearing impaired",
		"English No CC", "Not CC",
	} {
		if sdh(core.StreamInfo{Title: title}) {
			t.Errorf("sdh(%q)=true, want false (negated marker)", title)
		}
	}
	for _, title := range []string{
		"English (CC)", "English CC", "CC", "English [CC]", "(CC) English",
		"English (SDH)", "English SDH", "Hearing Impaired",
	} {
		if !sdh(core.StreamInfo{Title: title}) {
			t.Errorf("sdh(%q)=false, want true", title)
		}
	}
}

// 3: style=="sdh" explícito vence memória SDH=false nas duas ordens;
// memória SDH=true continua vencendo; sem style, memória continua mandando.
func TestPickStyleSDHBeatsPlainMemoryFalse(t *testing.T) {
	plain := core.StreamInfo{Type: "subtitle", Language: "en", Title: "English"}
	sdhTrack := core.StreamInfo{Type: "subtitle", Language: "en", Title: "English (SDH)"}
	memFalse := &Choice{Language: "en", SDH: false}
	if got := pick([]core.StreamInfo{plain, sdhTrack}, "subtitle", []string{"en"}, "sdh", false, memFalse); got != 1 {
		t.Fatalf("[plain,SDH] style=sdh mem=false: got %d want 1 (SDH)", got)
	}
	if got := pick([]core.StreamInfo{sdhTrack, plain}, "subtitle", []string{"en"}, "sdh", false, memFalse); got != 0 {
		t.Fatalf("[SDH,plain] style=sdh mem=false: got %d want 0 (SDH)", got)
	}
	memTrue := &Choice{Language: "en", SDH: true}
	if got := pick([]core.StreamInfo{plain, sdhTrack}, "subtitle", []string{"en"}, "sdh", false, memTrue); got != 1 {
		t.Fatalf("[plain,SDH] style=sdh mem=true: got %d want 1 (SDH)", got)
	}
	if got := pick([]core.StreamInfo{sdhTrack, plain}, "subtitle", []string{"en"}, "sdh", false, memTrue); got != 0 {
		t.Fatalf("[SDH,plain] style=sdh mem=true: got %d want 0 (SDH)", got)
	}
	// Sem style sdh, a memória (SDH=false -> plain) continua mandando.
	if got := pick([]core.StreamInfo{sdhTrack, plain}, "subtitle", []string{"en"}, "normal", false, memFalse); got != 1 {
		t.Fatalf("[SDH,plain] style=normal mem=false: got %d want 1 (plain, memória)", got)
	}
	if got := pick([]core.StreamInfo{plain, sdhTrack}, "subtitle", []string{"en"}, "normal", false, memTrue); got != 1 {
		t.Fatalf("[plain,SDH] style=normal mem=true: got %d want 1 (SDH, memória)", got)
	}
}

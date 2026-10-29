package subtitles

import (
	"fmt"
	"strings"
	"testing"
)

func assWith(lines ...string) string {
	var b strings.Builder
	b.WriteString("[Script Info]\nTitle: f\nScriptType: v4.00+\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
	for i, ln := range lines {
		fmt.Fprintf(&b, "Dialogue: 0,0:00:0%d.00,0:00:0%d.50,Default,,0,0,0,,%s\n", i+1, i+1, ln)
	}
	return b.String()
}

func srtWith(lines ...string) string {
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%d\n00:00:0%d,000 --> 00:00:0%d,500\n%s\n\n", i+1, i+1, i+1, ln)
	}
	return b.String()
}

// 4a: entrada cujo nome casa o idioma pedido vence a prioridade de
// formato (.ass pt-br ganha de .srt genérico; pob/pt_br idem entre .srt).
func TestUnzipPrefersFilenameLanguageOverFormat(t *testing.T) {
	raw := makeSubtitleZip(t,
		[2]string{"movie.srt", srtWith("GENERIC")},
		[2]string{"movie.pt-br.ass", assWith("PTBRASS")},
	)
	got, err := unzipSubtitle(raw, "pt-BR")
	if err != nil || !strings.Contains(string(got), "PTBRASS") {
		t.Fatalf("pt-br .ass must beat generic .srt: %q %v", got, err)
	}
	for _, name := range []string{"movie.pob.srt", "movie.pt_br.srt", "movie.pt-br.srt"} {
		raw := makeSubtitleZip(t,
			[2]string{"movie.srt", srtWith("GENERIC")},
			[2]string{name, srtWith("PTBRNAMED")},
		)
		got, err := unzipSubtitle(raw, "pt-BR")
		if err != nil || !strings.Contains(string(got), "PTBRNAMED") {
			t.Fatalf("%s must beat generic .srt for pt-BR: %q %v", name, got, err)
		}
	}
}

// 4b: orçamento agregado — após 16 entradas tentadas (ou 32MiB
// expandidos) para de tentar; uma entrada boa além do teto não é lida.
func TestUnzipBudgetStopsTryingEntries(t *testing.T) {
	var entries [][2]string
	for i := 0; i < 20; i++ {
		entries = append(entries, [2]string{fmt.Sprintf("junk%02d.srt", i), "not a subtitle at all"})
	}
	entries = append(entries, [2]string{"good.srt", srtWith("TOOFAR")})
	raw := makeSubtitleZip(t, entries...)
	if _, err := unzipSubtitle(raw, "en"); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("many junk entries must exhaust the try budget, got %v", err)
	}

	// Orçamento em bytes expandidos: entradas lixo grandes consomem o
	// teto antes da boa.
	bigJunk := strings.Repeat("x", 5<<20)
	var entries2 [][2]string
	for i := 0; i < 8; i++ {
		entries2 = append(entries2, [2]string{fmt.Sprintf("big%02d.srt", i), bigJunk})
	}
	entries2 = append(entries2, [2]string{"good.srt", srtWith("TOOFARBYTES")})
	raw = makeSubtitleZip(t, entries2...)
	if _, err := unzipSubtitle(raw, "en"); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expanded-bytes budget must stop before the good entry, got %v", err)
	}
}

// Contagem é de cues parseados, não de "-->" crus: setas dentro do
// texto de um diálogo não inflam a contagem (mutação: strings.Count
// sobre o SRT normalizado coroaria o stub de 1 cue com setas no texto).
func TestUnzipCueCountUsesParsedCuesNotArrowText(t *testing.T) {
	// ASS de 1 diálogo com "-->" no texto NÃO vence SRT de 3 cues.
	raw := makeSubtitleZip(t,
		[2]string{"movie.srt", srtWith("SRTUM", "SRTDOIS", "SRTTRES")},
		[2]string{"movie.ass", assWith("ASSARROW --> --> --> -->")},
	)
	got, err := unzipSubtitle(raw, "en")
	if err != nil || !strings.Contains(string(got), "SRTTRES") || strings.Contains(string(got), "ASSARROW") {
		t.Fatalf("3-cue .srt must beat 1-dialogue .ass with arrows in text: %q %v", got, err)
	}
	// Reverso: SRT de 1 cue com "-->" no texto NÃO vence ASS de 3.
	raw = makeSubtitleZip(t,
		[2]string{"movie.srt", srtWith("SRTARROW --> --> --> -->")},
		[2]string{"movie.ass", assWith("ASSUM", "ASSDOIS", "ASSTRES")},
	)
	got, err = unzipSubtitle(raw, "en")
	if err != nil || !strings.Contains(string(got), "ASSTRES") || strings.Contains(string(got), "SRTARROW") {
		t.Fatalf("3-dialogue .ass must beat 1-cue .srt with arrows in text: %q %v", got, err)
	}
}

// 4c: .srt de 1 cue perde para .ass completo (mais cues) quando ambos
// parseiam; empate de cues mantém a prioridade de formato (.srt).
func TestUnzipPrefersMoreCuesOverSingleCueSRT(t *testing.T) {
	raw := makeSubtitleZip(t,
		[2]string{"movie.srt", srtWith("ONLYONE")},
		[2]string{"movie.ass", assWith("FULL1", "FULL2", "FULL3")},
	)
	got, err := unzipSubtitle(raw, "en")
	if err != nil || !strings.Contains(string(got), "FULL3") {
		t.Fatalf("complete .ass must beat 1-cue .srt: %q %v", got, err)
	}
	raw = makeSubtitleZip(t,
		[2]string{"movie.ass", assWith("ASS ONE")},
		[2]string{"movie.srt", srtWith("SRTONE")},
	)
	got, err = unzipSubtitle(raw, "en")
	if err != nil || !strings.Contains(string(got), "SRTONE") {
		t.Fatalf("cue tie must keep .srt format preference: %q %v", got, err)
	}
}

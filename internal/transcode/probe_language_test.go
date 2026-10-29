package transcode

import (
	"testing"
)

// Tabela do parser BCP47 único: script sem região usa a convenção
// Hans->cn / Hant->tw; região explícita sempre vence o script; script
// Latn é ignorado; só pt/zh preservam região, o resto cai no código
// primário (na forma 639-2 que os probes já reportavam).
func TestNormalizeBCP47Language(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"zh-Hant-TW", "zh-tw"},
		{"zh-Hans", "zh-cn"},
		{"zh-Hant", "zh-tw"},
		{"zh-Hans-HK", "zh-hk"}, // região explícita vence o script
		{"zh-Hant-CN", "zh-cn"}, // região explícita vence o script
		{"pt-Latn-BR", "pt-br"},
		{"pt-BR", "pt-br"},
		{"pt-PT", "pt-pt"},
		{"pt", "por"},
		{"zh", "chi"},
		{"en-US", "eng"},
		{"en-GB", "eng"},
		{"fr-CA", "fre"},
		{"es-419", "spa"},
		{"de-CH-1901", "ger"},
		{"en_US", "eng"},
		{"PT-br", "pt-br"},
		{"und", ""},
		{"", ""},
		// Primário inválido é lixo, não língua: "" faz o caller cair
		// no language/mdhd legado.
		{"p", ""},
		{"\xff\xfe", ""},
		{"p1", ""},
		{"port", ""}, // primário >3 letras foge do 639-1/639-2 aceito
		{"x-klingon", ""},
		// 639-2 de 3 letras válido continua passando.
		{"eng", "eng"},
		{"deu", "deu"},
	} {
		if got := normalizeBCP47Language(c.in); got != c.want {
			t.Errorf("normalizeBCP47Language(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// 1a: MKV — LanguageBCP47 presente VENCE o legacy Language (mkvmerge
// grava ambos; o legacy pode estar genérico/errado).
func TestMKVBCP47WinsOverLegacyLanguage(t *testing.T) {
	for _, c := range []struct {
		track mkvTrack
		want  string
	}{
		{mkvTrack{language: "eng", hasLanguage: true, bcp47: "pt-BR"}, "pt-br"},
		{mkvTrack{language: "por", hasLanguage: true, bcp47: "en-US"}, "eng"},
		{mkvTrack{language: "eng", hasLanguage: true, bcp47: "zh-Hant-TW"}, "zh-tw"},
		{mkvTrack{language: "chi", hasLanguage: true, bcp47: "zh-Hans"}, "zh-cn"},
		{mkvTrack{language: "por", hasLanguage: true, bcp47: "pt-Latn-BR"}, "pt-br"},
		// Sem BCP47, o legacy continua valendo.
		{mkvTrack{language: "por", hasLanguage: true}, "por"},
	} {
		c.track.kind, c.track.codecID, c.track.channels, c.track.sampling = 2, "A_AC3", 2, 48000
		si, ok, err := c.track.stream(1)
		if err != nil || !ok || si.Language != c.want {
			t.Errorf("%+v: language %q (ok %v, err %v), want %q", c.track, si.Language, ok, err, c.want)
		}
	}
}

// 1b: ffprobe — tag language_bcp47 (qualquer caixa) vence language.
// Em MKV real quem carrega a região é o native probe (elemento
// LanguageBCP47); este branch do ffprobe é só o fallback para o que o
// native probe adia. A chave maiúscula LANGUAGE_BCP47 é a forma real
// emitida pelo ffprobe 8.x medido.
func TestParseProbePrefersLanguageBCP47Tag(t *testing.T) {
	for _, c := range []struct {
		name string
		tags string
		want string
	}{
		{"lower", `"language":"por","language_bcp47":"pt-PT"`, "pt-pt"},
		{"upper key", `"language":"eng","LANGUAGE_BCP47":"zh-Hant-TW"`, "zh-tw"},
		{"upper key pt-BR (ffprobe 8.x)", `"language":"por","LANGUAGE_BCP47":"pt-BR"`, "pt-br"},
		{"mixed key", `"language":"eng","Language_Bcp47":"zh-Hans"`, "zh-cn"},
		{"only legacy", `"language":"eng"`, "eng"},
		{"empty bcp47 falls back", `"language":"spa","language_bcp47":""`, "spa"},
	} {
		js := `{"streams":[{"index":0,"codec_type":"subtitle","codec_name":"subrip","tags":{` + c.tags + `}}],"format":{"format_name":"matroska","duration":"10"}}`
		m, err := ParseProbe([]byte(js))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(m.Streams) != 1 || m.Streams[0].Language != c.want {
			t.Errorf("%s: streams=%+v want language %q", c.name, m.Streams, c.want)
		}
	}
}

// 1c: MP4 — box elng (em mdia) fornece o BCP47; sem elng, o mdhd
// 639-2 é mantido (região genuinamente desconhecida).
func TestMP4ElngWinsOverMdhdLanguage(t *testing.T) {
	build := func(elng string) []byte {
		// mdhd: timescale 48000, duration 48000, language packed "por" (0x41f2).
		mdhd := mkFullBox("mdhd", 0, 0, u32(0), u32(0), u32(48000), u32(48000), u16(0x41f2), u16(0))
		hdlr := mkFullBox("hdlr", 0, 0, u32(0), []byte("soun"), make([]byte, 12), []byte("Handler\x00"))
		stbl := mkBox("stbl", mkFullBox("stsd", 0, 0, u32(1), audioEntry("mp4a", 2, 48000, esdsAAC(2))))
		mdiaParts := [][]byte{mdhd, hdlr}
		if elng != "" {
			mdiaParts = append(mdiaParts, mkFullBox("elng", 0, 0, []byte(elng+"\x00")))
		}
		mdiaParts = append(mdiaParts, mkBox("minf", stbl))
		return mkBox("trak", mkBox("mdia", mdiaParts...))
	}
	for _, c := range []struct {
		name string
		elng string
		want string
	}{
		{"elng pt-BR vence mdhd por", "pt-BR", "pt-br"},
		{"elng zh-Hant-TW", "zh-Hant-TW", "zh-tw"},
		{"elng lixo curto mantém mdhd por", "p", "por"},
		{"elng binário mantém mdhd por", "\xff\xfe", "por"},
		{"sem elng mantém 639-2 do mdhd", "", "por"},
	} {
		trak := build(c.elng)
		boxes, err := readBoxes(trak, 0, len(trak))
		if err != nil || len(boxes) != 1 {
			t.Fatalf("%s: readBoxes: %v", c.name, err)
		}
		si, ok, err := mp4Track(trak, boxes[0], 1000)
		if err != nil || !ok {
			t.Fatalf("%s: mp4Track: ok=%v err=%v", c.name, ok, err)
		}
		if si.Language != c.want {
			t.Errorf("%s: language=%q want %q", c.name, si.Language, c.want)
		}
	}
}

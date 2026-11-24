package nzb

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const sampleNZB = `<?xml version="1.0" encoding="iso-8859-1" ?>
<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <head>
   <meta type="title">Some.Movie.2024.1080p.WEB-DL</meta>
   <meta type="password">s3cr&amp;t</meta>
   <meta type="tag">HD</meta>
 </head>
 <file poster="poster@example.com" date="1700000000" subject="[1/3] - &quot;Some.Movie.2024.part01.rar&quot; yEnc (1/4)">
  <groups>
   <group>alt.binaries.test</group>
   <group>alt.binaries.test</group>
   <group>alt.binaries.misc</group>
  </groups>
  <segments>
   <segment bytes="1000" number="3">p1s3@example</segment>
   <segment bytes="1000" number="1">&lt;p1s1@example&gt;</segment>
   <segment bytes="1000" number="2">p1s2@example</segment>
   <segment bytes="1000" number="2">p1s2dup@example</segment>
   <segment bytes="500" number="4">p1s4@example</segment>
  </segments>
 </file>
 <file poster="poster@example.com" date="1700000001" subject="[2/3] - &quot;Some.Movie.2024.part02.rar&quot; yEnc (1/5)">
  <groups><group>alt.binaries.test</group></groups>
  <segments>
   <segment bytes="1000" number="1">p2s1@example</segment>
   <segment bytes="1000" number="2">p2s2@example</segment>
   <segment bytes="1000" number="4">p2s4@example</segment>
  </segments>
 </file>
 <file poster="poster@example.com" date="1700000002" subject="[3/3] - &quot;Some.Movie.2024.par2&quot; yEnc (1/1)">
  <groups><group>alt.binaries.test</group></groups>
  <segments><segment bytes="100" number="1">par@example</segment></segments>
 </file>
 <file poster="x" date="1" subject="empty file">
  <groups><group>alt.binaries.test</group></groups>
  <segments></segments>
 </file>
</nzb>`

func TestParse(t *testing.T) {
	n, err := Parse([]byte(sampleNZB))
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Password(); got != "s3cr&t" {
		t.Errorf("password = %q", got)
	}
	if got := n.MetaValue("TITLE"); got != "Some.Movie.2024.1080p.WEB-DL" {
		t.Errorf("title = %q", got)
	}
	if len(n.Files) != 3 {
		t.Fatalf("files = %d, want 3 (empty file dropped)", len(n.Files))
	}
	f := n.Files[0]
	if f.Name != "Some.Movie.2024.part01.rar" {
		t.Errorf("name = %q", f.Name)
	}
	if f.Info.Kind != KindRAR || f.Info.Volume != 0 || !f.Info.NewStyle {
		t.Errorf("info = %+v", f.Info)
	}
	if !reflect.DeepEqual(f.Groups, []string{"alt.binaries.test", "alt.binaries.misc"}) {
		t.Errorf("groups = %v", f.Groups)
	}
	if !f.Date.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("date = %v", f.Date)
	}
	var ids []string
	for _, s := range f.Segments {
		ids = append(ids, s.MessageID)
	}
	if !reflect.DeepEqual(ids, []string{"p1s1@example", "p1s2@example", "p1s3@example", "p1s4@example"}) {
		t.Errorf("segments = %v", ids)
	}
	if f.Duplicates != 1 {
		t.Errorf("duplicates = %d", f.Duplicates)
	}
	if f.Bytes() != 3500 {
		t.Errorf("bytes = %d", f.Bytes())
	}
	if f.SubjectIndex != 1 || f.SubjectCount != 3 || f.ExpectedSegments != 4 {
		t.Errorf("counters = %d/%d seg %d", f.SubjectIndex, f.SubjectCount, f.ExpectedSegments)
	}
	if m := f.Missing(); len(m) != 0 {
		t.Errorf("missing = %v", m)
	}
	// File 2 lacks segment 3 and (per subject) 5.
	if m := n.Files[1].Missing(); !reflect.DeepEqual(m, []int{3, 5}) {
		t.Errorf("missing = %v", m)
	}
	if n.Files[2].Info.Kind != KindPar2 {
		t.Errorf("par2 kind = %v", n.Files[2].Info.Kind)
	}
	if n.TotalBytes() != 3500+3000+100 {
		t.Errorf("total = %d", n.TotalBytes())
	}
}

func TestParseNoNamespaceAndUTF8(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?><nzb><file subject="Fïlm.mkv (1/2)"><groups><group>a.b</group></groups>` +
		`<segments><segment bytes="10" number="1">a@b</segment><segment bytes="10" number="2">c@d</segment></segments></file></nzb>`
	n, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if n.Files[0].Name != "Fïlm.mkv" || n.Files[0].Info.Kind != KindVideo {
		t.Errorf("file = %q %v", n.Files[0].Name, n.Files[0].Info.Kind)
	}
}

func TestParseLatin1(t *testing.T) {
	doc := "<?xml version=\"1.0\" encoding=\"iso-8859-1\"?><nzb><file subject=\"Caf\xe9.mkv\"><groups><group>a.b</group></groups>" +
		"<segments><segment bytes=\"10\" number=\"1\">a@b</segment></segments></file></nzb>"
	n, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if n.Files[0].Name != "Café.mkv" {
		t.Errorf("name = %q", n.Files[0].Name)
	}
}

func TestParseZeroBasedNumbers(t *testing.T) {
	doc := `<nzb><file subject="x.mkv"><segments><segment number="0">a@b</segment><segment number="1">c@d</segment></segments></file></nzb>`
	n, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if n.Files[0].Segments[0].Number != 1 || n.Files[0].Segments[1].Number != 2 {
		t.Errorf("segments = %+v", n.Files[0].Segments)
	}
}

func TestParseInvalid(t *testing.T) {
	for _, doc := range []string{"", "not xml", "<nzb></nzb>", "<nzb><file subject='a'><segments/></file></nzb>"} {
		if _, err := Parse([]byte(doc)); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) err = %v, want ErrInvalid", doc, err)
		}
	}
}

func TestNameFromSubject(t *testing.T) {
	cases := map[string]string{
		`"Movie.2024.part01.rar" yEnc (1/50)`:                                       "Movie.2024.part01.rar",
		`[01/42] - "Movie.2024.mkv" yEnc (1/1000)`:                                  "Movie.2024.mkv",
		`Movie 2024 [03/42] - "Movie.2024.r01" yEnc (12/50)`:                        "Movie.2024.r01",
		`[PRiVATE]-[WtFnZb]-[Movie.2024.mkv]-[1/5] - "" yEnc 1234 (1/40)`:           "Movie.2024.mkv",
		`[N3wZ] \6aB1c\::[PRiVATE]-[WtFnZb]-[24]-[1/Movie.2024.mkv] - "" yEnc`:      "Movie.2024.mkv",
		`[PRiVATE]-[WtFnZb]-[1/5]-[Show_[SubsPlease]_[1080p].mkv] - "" yEnc (1/40)`: "Show_[SubsPlease]_[1080p].mkv",
		`[PRiVATE]-[WtFnZb]-[Show_[Group].part01.rar]-[1/10] - "" yEnc (1/40)`:      "Show_[Group].part01.rar",
		`Movie.2024.r00 (1/50)`:                                                     "Movie.2024.r00",
		`Movie.2024.7z.001 yEnc (1/50)`:                                             "Movie.2024.7z.001",
		`Re: Movie.2024.mkv - 1.2 GB - yEnc (1/50)`:                                 "Movie.2024.mkv",
		`"Movie.2024.vol03+04.par2" yEnc (1/4)`:                                     "Movie.2024.vol03+04.par2",
		`"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6" yEnc (1/50)`:                            "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
		`"folder/sub/Movie.mkv" yEnc (1/2)`:                                         "Movie.mkv",
		`(Movie) "Movie.2024.nfo" yEnc (1/1)`:                                       "Movie.2024.nfo",
	}
	for subj, want := range cases {
		if got := NameFromSubject(subj); got != want {
			t.Errorf("NameFromSubject(%q) = %q, want %q", subj, got, want)
		}
	}
}

// U+0250 (ɐ) upper-cases to U+2C6F, two bytes to three, so an offset found in
// an upper-cased copy points past the marker in the subject itself. U+017F (ſ)
// upper-cases to S, two bytes to one, so the offset points before it.
func TestParsePrivateFrameMarkerOffset(t *testing.T) {
	cases := []struct {
		name, subject, frame, want string
	}{
		{"growing letter, nothing after marker", "ɐ[PRiVATE]-", "", "ɐ[PRiVATE]-"},
		{"two growing letters, one byte after marker", "ɐɐ[PRiVATE]-x", "", "ɐɐ[PRiVATE]-x"},
		{"growing letters, nested name no later step recovers", "ɐɐ[PRiVATE]-[Grp]-[Show_[Grp].mkv]", "Show_[Grp].mkv", "Show_[Grp].mkv"},
		{"growing letters, plain name the bracket step recovers", "ɐɐɐɐ[PRiVATE]-[Grp]-[Show.2024.mkv]-[1/5]", "Show.2024.mkv", "Show.2024.mkv"},
		{"shrinking letters, nested name", "ſſ[PRiVATE]-[Grp]-[Show_[Grp].mkv]", "Show_[Grp].mkv", "Show_[Grp].mkv"},
		{"mixed-case marker, ASCII only", "[pRiVaTe]-[Grp]-[Show_[Grp].mkv]", "Show_[Grp].mkv", "Show_[Grp].mkv"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePrivateFrame(tc.subject); got != tc.frame {
				t.Errorf("parsePrivateFrame(%q) = %q, want %q", tc.subject, got, tc.frame)
			}
			if got := NameFromSubject(tc.subject); got != tc.want {
				t.Errorf("NameFromSubject(%q) = %q, want %q", tc.subject, got, tc.want)
			}
		})
	}
}

func TestParsePrivateFrameMarkerOffsetNZB(t *testing.T) {
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="poster@example.invalid" date="1700000000" subject="ɐ[PRiVATE]-">
  <groups><group>alt.binaries.example</group></groups>
  <segments><segment bytes="1000" number="1">part1@example.invalid</segment></segments>
 </file>
 <file poster="poster@example.invalid" date="1700000001" subject="ɐɐ[PRiVATE]-[Grp]-[Show_[Grp].mkv]">
  <groups><group>alt.binaries.example</group></groups>
  <segments><segment bytes="1000" number="1">part2@example.invalid</segment></segments>
 </file>
</nzb>`
	n, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var names []string
	for _, f := range n.Files {
		names = append(names, f.Name)
	}
	want := []string{"ɐ[PRiVATE]-", "Show_[Grp].mkv"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("file names = %q, want %q", names, want)
	}
}

func TestNameFromSubjectRealWorldUnquoted(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    string
	}{
		{
			name:    "quoted with spaces",
			subject: `"Concert Recording 2019 Full Show.mkv" yEnc (1/120)`,
			want:    "Concert Recording 2019 Full Show.mkv",
		},
		{
			name:    "unquoted with spaces and segment counter",
			subject: `Concert Recording 2019 Full Show.mkv (1/120)`,
			want:    "Concert Recording 2019 Full Show.mkv",
		},
		{
			name:    "unquoted with spaces and hyphen in title",
			subject: `Some Show - Episode Title 1080p.mp4 yEnc (1/50)`,
			want:    "Some Show - Episode Title 1080p.mp4",
		},
		{
			name:    "unquoted with parens and bracket tag",
			subject: `My Holiday Video (Part 2) [Extended].mp4 yEnc (1/80)`,
			want:    "My Holiday Video (Part 2) [Extended].mp4",
		},
		{
			name:    "leading bracket counter with dash",
			subject: `[1/50] - name.ext yEnc (1/50)`,
			want:    "name.ext",
		},
		{
			name:    "leading bracket counter with spaces in filename",
			subject: `[01/42] - Concert Recording 2019 Full Show.mkv yEnc (1/120)`,
			want:    "Concert Recording 2019 Full Show.mkv",
		},
		{
			name:    "leading counter with spaces in multipart rar",
			subject: `[03/20] - Some Show - Episode Title.part03.rar yEnc (1/50)`,
			want:    "Some Show - Episode Title.part03.rar",
		},
		{
			name:    "post title and mid counter before unquoted filename",
			subject: `Movie 2024 [03/42] - Movie 2024 Part 3.r01 yEnc (12/50)`,
			want:    "Movie 2024 Part 3.r01",
		},
		{
			name:    "leading paren counter and of wording",
			subject: `(01 of 10) - Artist Name - 01 Track Title.flac yEnc (1/20)`,
			want:    "Artist Name - 01 Track Title.flac",
		},
		{
			name:    "bare leading counter without brackets",
			subject: `01/10 - Concert Recording 2019.7z.001 yEnc (1/50)`,
			want:    "Concert Recording 2019.7z.001",
		},
		{
			name:    "reply prefix plus counter and size noise",
			subject: `Re: [01/10] - Concert Recording 2019 Full Show.mkv - 1.2 GB - yEnc (1/50)`,
			want:    "Concert Recording 2019 Full Show.mkv",
		},
		{
			name:    "mid-title of wording is preserved",
			subject: `Book 1 of 3 - The Beginning.epub yEnc (1/5)`,
			want:    "Book 1 of 3 - The Beginning.epub",
		},
		{
			name:    "mid-title size token is preserved",
			subject: `Linux Live 4GB Edition.iso yEnc (1/50)`,
			want:    "Linux Live 4GB Edition.iso",
		},
		{
			name:    "embedded dotted codec bracket does not steal unquoted filename",
			subject: `Show Name [H.264] [1080p].mkv yEnc (1/50)`,
			want:    "Show Name [H.264] [1080p].mkv",
		},
		{
			name:    "yEnc with byte size before segment counter",
			subject: `Concert Recording 2019 Full Show.mkv yEnc 52428800 (1/120)`,
			want:    "Concert Recording 2019 Full Show.mkv",
		},
		{
			name:    "segment counter before yEnc",
			subject: `Concert Recording 2019 Full Show.mkv (1/120) yEnc`,
			want:    "Concert Recording 2019 Full Show.mkv",
		},
		{
			name:    "plain filename ending in digits without yEnc trailer",
			subject: `Concert Recording 2019.mp4`,
			want:    "Concert Recording 2019.mp4",
		},
		{
			name:    "plain 7z split without yEnc trailer",
			subject: `Concert Recording 2019.7z.001`,
			want:    "Concert Recording 2019.7z.001",
		},
		{
			name:    "standalone bracketed filename still extracted",
			subject: `[#a.b.moovee@EFNet]-[FULL]-[Movie.2024.part01.rar] yEnc (1/50)`,
			want:    "Movie.2024.part01.rar",
		},
		{
			name:    "second quote with known extension beats dotted first quote",
			subject: `Post "Mr. Smith" - "Movie.2024.mkv" yEnc (1/50)`,
			want:    "Movie.2024.mkv",
		},
		{
			name:    "trailing promo domain does not override real extension",
			subject: `Concert Recording 2019.mkv - Visit Site.com yEnc (1/50)`,
			want:    "Concert Recording 2019.mkv",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NameFromSubject(tc.subject); got != tc.want {
				t.Errorf("NameFromSubject(%q) = %q, want %q", tc.subject, got, tc.want)
			}
		})
	}
}

func TestSubjectCounters(t *testing.T) {
	i, n := subjectFileCounter(`[03/42] - "Movie.r01" yEnc (12/50)`)
	if i != 3 || n != 42 {
		t.Errorf("file counter = %d/%d", i, n)
	}
	if i, n := subjectFileCounter(`"Movie.r01" yEnc (12/50)`); i != 0 || n != 0 {
		t.Errorf("file counter = %d/%d, want none", i, n)
	}
	if got := subjectSegmentCount(`"Movie.r01" yEnc (12/50)`); got != 50 {
		t.Errorf("segment count = %d", got)
	}
	if got := subjectSegmentCount(`"Movie.r01" yEnc (1/50) 123456`); got != 50 {
		t.Errorf("segment count with size = %d", got)
	}
}

func TestClassify(t *testing.T) {
	type want struct {
		kind     Kind
		vol      int
		set, ext string
		newStyle bool
	}
	cases := map[string]want{
		"Movie.2024.mkv":            {KindVideo, -1, "movie.2024", "mkv", false},
		"Movie.2024.part01.rar":     {KindRAR, 0, "movie.2024", "rar", true},
		"Movie.2024.part1.rar":      {KindRAR, 0, "movie.2024", "rar", true},
		"Movie.2024.part010.rar":    {KindRAR, 9, "movie.2024", "rar", true},
		"Movie.2024.rar":            {KindRAR, 0, "movie.2024", "rar", false},
		"Movie.2024.r00":            {KindRAR, 1, "movie.2024", "rar", false},
		"Movie.2024.r99":            {KindRAR, 100, "movie.2024", "rar", false},
		"Movie.2024.s00":            {KindRAR, 101, "movie.2024", "rar", false},
		"Movie.2024.7z":             {KindSevenZip, 0, "movie.2024", "7z", false},
		"Movie.2024.7z.001":         {KindSevenZip, 0, "movie.2024", "7z", false},
		"Movie.2024.7z.012":         {KindSevenZip, 11, "movie.2024", "7z", false},
		"Movie.2024.zip":            {KindZIP, 0, "movie.2024", "zip", false},
		"Movie.2024.zip.001":        {KindZIP, 0, "movie.2024", "zip", false},
		"Movie.2024.zip.003":        {KindZIP, 2, "movie.2024", "zip", false},
		"Movie.2024.z01":            {KindZIP, 0, "movie.2024", "zip", true},
		"Movie.2024.z02":            {KindZIP, 1, "movie.2024", "zip", true},
		"Movie.2024.mkv.001":        {KindSplit, 0, "movie.2024.mkv", "mkv", false},
		"Movie.2024.002":            {KindSplit, 1, "movie.2024", "", false},
		"Movie.2024.par2":           {KindPar2, -1, "movie.2024", "par2", false},
		"Movie.2024.vol07+08.PAR2":  {KindPar2, -1, "movie.2024", "par2", false},
		"Movie.2024.nfo":            {KindNFO, -1, "movie.2024", "nfo", false},
		"Movie.2024.sfv":            {KindSFV, -1, "movie.2024", "sfv", false},
		"Movie.2024.en.srt":         {KindSubtitle, -1, "movie.2024.en", "srt", false},
		"cover.jpg":                 {KindImage, -1, "cover", "jpg", false},
		"Movie.2024.iso":            {KindISO, -1, "movie.2024", "iso", false},
		"a1b2c3d4e5f6":              {KindUnknown, -1, "a1b2c3d4e5f6", "", false},
		"Movie.2024.M2TS":           {KindVideo, -1, "movie.2024", "m2ts", false},
		"Show.S01E02.720p.HDTV.ts":  {KindVideo, -1, "show.s01e02.720p.hdtv", "ts", false},
		`Movie\Sample\sample-x.mkv`: {KindVideo, -1, "sample-x", "mkv", false},
	}
	for name, w := range cases {
		got := Classify(name)
		if got.Kind != w.kind || got.Volume != w.vol || got.SetName != w.set || got.Ext != w.ext || got.NewStyle != w.newStyle {
			t.Errorf("Classify(%q) = %+v, want %+v", name, got, w)
		}
	}
	for name, sample := range map[string]bool{
		"movie-sample.mkv":         true,
		"Sample/movie.mkv":         true,
		"movie.proof.jpg":          true,
		"Samplers.Unite.2024.mkv":  false,
		"Movie.2024.1080p.mkv":     false,
		"movie.2024.SAMPLE.mkv":    true,
		"The.Sample.Size.2024.mkv": true, // an accepted false positive; size filters disambiguate
	} {
		if got := Classify(name).Sample; got != sample {
			t.Errorf("Classify(%q).Sample = %v, want %v", name, got, sample)
		}
	}
	for name, extra := range map[string]bool{
		"Movie.Trailer.mkv":            true,
		"Featurettes/Making.Of.mkv":    true,
		"Movie.2024.Behind.The.Scenes": true,
		"Movie.2024.1080p.mkv":         false,
	} {
		if got := Classify(name).Extra; got != extra {
			t.Errorf("Classify(%q).Extra = %v, want %v", name, got, extra)
		}
	}
}

func TestIsObfuscated(t *testing.T) {
	cases := map[string]bool{
		"Movie.2024.1080p.WEB-DL.mkv":                      false,
		"Movie.2024.part01.rar":                            false,
		"movie.r00":                                        false,
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6":                 true,
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.mkv":             true,
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6.part01.rar":      true,
		"a1b2c3d4e5f6a7b8.mkv":                             true,
		"a1b2.c3d4.e5f6.7890.abcd.ef01.mkv":                true,
		"[Group]-b2.bef89a622e4a23f07b0d3757ad5e8a.a0.mkv": true,
		"8f14e45f-ceea-467f-a8c4-2b6a8b2e7c11.mkv":         true,
		"Xk3Pq9LmZr7Tb2Wc.rar":                             true,
		"abc.xyz.12345.mkv":                                true,
		"b00bs.12345.mkv":                                  true,
		"Backup_72019S01-02.mkv":                           true,
		"noextension":                                      true,
		"":                                                 true,
		"TheMatrixReloaded.mkv":                            false,
		"Movie2024.mkv":                                    false,
		"Show.S01E01.ts":                                   false,
	}
	for name, want := range cases {
		if got := IsObfuscated(name); got != want {
			t.Errorf("IsObfuscated(%q) = %v, want %v", name, got, want)
		}
	}
}

func fileNamed(name string, bytes int64) *File {
	return &File{Name: name, Info: Classify(name), Segments: []Segment{{Number: 1, Bytes: bytes, MessageID: name}}}
}

func TestGroupSets(t *testing.T) {
	files := []*File{
		fileNamed("Movie.r01", 100),
		fileNamed("Movie.rar", 100),
		fileNamed("Movie.r00", 100),
		fileNamed("Movie.r03", 50),
		fileNamed("Movie.r00", 90), // smaller repost loses
		fileNamed("Subs.part02.rar", 10),
		fileNamed("Subs.part01.rar", 10),
		fileNamed("Movie.mkv", 1),
		fileNamed("Other.7z.002", 5),
		fileNamed("Other.7z.001", 5),
		fileNamed("Split.mkv.000", 7),
		fileNamed("Split.mkv.001", 7),
	}
	sets := GroupSets(files)
	if len(sets) != 4 {
		t.Fatalf("sets = %d", len(sets))
	}
	movie := sets[0]
	if movie.Kind != KindRAR || movie.Name != "movie" || movie.NewStyle {
		t.Fatalf("first set = %+v", movie)
	}
	var names []string
	for _, v := range movie.Volumes {
		names = append(names, v.Name)
	}
	if !reflect.DeepEqual(names, []string{"Movie.rar", "Movie.r00", "Movie.r01", "Movie.r03"}) {
		t.Errorf("order = %v", names)
	}
	if !reflect.DeepEqual(movie.Missing, []int{3}) {
		t.Errorf("missing = %v", movie.Missing)
	}
	if len(movie.Duplicates) != 1 || movie.Duplicates[0].Bytes() != 90 {
		t.Errorf("duplicates = %v", movie.Duplicates)
	}
	if movie.Complete() {
		t.Error("complete")
	}
	for _, s := range sets[1:] {
		if !s.Complete() || len(s.Volumes) != 2 {
			t.Errorf("set %s: %+v", s.Name, s)
		}
		if s.Volumes[0].Info.Volume >= s.Volumes[1].Info.Volume {
			t.Errorf("set %s volumes out of order", s.Name)
		}
	}
	names = names[:0]
	for _, s := range sets {
		names = append(names, s.Kind.String()+":"+s.Name)
	}
	if !reflect.DeepEqual(names, []string{"rar:movie", "rar:subs", "split:split.mkv", "7z:other"}) {
		t.Errorf("sets = %v", names)
	}
}

func TestGroupSetsZSplit(t *testing.T) {
	files := []*File{
		fileNamed("Pack.zip", 50),
		fileNamed("Pack.z02", 100),
		fileNamed("Pack.z01", 100),
	}
	sets := GroupSets(files)
	if len(sets) != 1 || !sets[0].Complete() {
		t.Fatalf("sets = %+v", sets)
	}
	var names []string
	for _, v := range sets[0].Volumes {
		names = append(names, v.Name)
	}
	if !reflect.DeepEqual(names, []string{"Pack.z01", "Pack.z02", "Pack.zip"}) {
		t.Fatalf("order = %v, want [Pack.z01 Pack.z02 Pack.zip]", names)
	}

	// Missing closing .zip volume must mark the WinZip split set incomplete.
	incomplete := GroupSets([]*File{
		fileNamed("Pack.z01", 100),
		fileNamed("Pack.z02", 100),
	})
	if len(incomplete) != 1 || incomplete[0].Complete() || !reflect.DeepEqual(incomplete[0].Missing, []int{2}) {
		t.Fatalf("zSplit without .zip = %+v, want Missing=[2]", incomplete)
	}
}

// GroupSets used to renumber the files it was given, so grouping the same
// files twice (the stream planner does, around obfuscation probing) slid a
// part00 set's reposted duplicate into another volume's slot.
func TestGroupSetsIsRepeatable(t *testing.T) {
	files := []*File{
		fileNamed("m.part00.rar", 50), // loses to the larger repost below
		fileNamed("m.part00.rar", 100),
		fileNamed("m.part01.rar", 100),
		fileNamed("m.part02.rar", 100),
		fileNamed("m.part02.rar", 60), // a smaller repost of part02
	}
	order := func() []*File {
		sets := GroupSets(files)
		if len(sets) != 1 || !sets[0].Complete() {
			t.Fatalf("sets = %+v", sets)
		}
		return sets[0].Volumes
	}
	first := order()
	want := []*File{files[1], files[2], files[3]}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("first grouping = %v", first)
	}
	for i := 0; i < 3; i++ {
		if got := order(); !reflect.DeepEqual(got, want) {
			t.Fatalf("grouping %d differs: %v", i+2, got)
		}
	}
}

func TestMissingFirstVolume(t *testing.T) {
	sets := GroupSets([]*File{fileNamed("m.part02.rar", 1), fileNamed("m.part03.rar", 1)})
	if len(sets) != 1 || !reflect.DeepEqual(sets[0].Missing, []int{0}) {
		t.Fatalf("sets = %+v", sets)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(sampleNZB))
	f.Add([]byte(`<nzb><file subject="x"><segments><segment number="1">a</segment></segments></file></nzb>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := Parse(data)
		if err != nil {
			return
		}
		for _, file := range n.Files {
			_ = file.Missing()
			if len(file.Segments) == 0 {
				t.Fatal("file without segments")
			}
			if strings.ContainsAny(file.Segments[0].MessageID, "<>") && len(file.Segments[0].MessageID) < 2 {
				t.Fatal("bad id")
			}
		}
		_ = n.ArchiveSets()
	})
}

func FuzzNameFromSubject(f *testing.F) {
	f.Add(`"Movie.2024.part01.rar" yEnc (1/50)`)
	f.Add(`[PRiVATE]-[WtFnZb]-[Movie.2024.mkv]-[1/5] - "" yEnc`)
	f.Fuzz(func(t *testing.T, s string) {
		_ = NameFromSubject(s)
		_ = IsObfuscated(s)
		_ = Classify(s)
		_, _ = subjectFileCounter(s)
	})
}

func TestPar2Index(t *testing.T) {
	mk := func(name string, bytes int64) *File {
		return &File{Name: name, Info: Classify(name), Segments: []Segment{{Number: 1, Bytes: bytes, MessageID: name}}}
	}
	files := []*File{
		mk("Movie.mkv", 1<<30),
		mk("Movie.vol00+01.par2", 100),
		mk("Movie.vol01+02.PAR2", 200),
		mk("Movie.par2", 5000),
		mk("Movie.sample.par2", 4000),
	}
	if got := Par2Index(files); got != files[3] {
		t.Fatalf("Par2Index = %v", got)
	}
	if got := Par2Index(files[:3]); got != nil {
		t.Fatalf("volumes only = %v", got.Name)
	}
	if !IsPar2Volume(`dir\x.vol12+34.par2`) || IsPar2Volume("x.par2") || IsPar2Volume("x.mkv") {
		t.Fatal("IsPar2Volume")
	}
}

// A hostile segment number must not size per-part tables (SegmentCount,
// Missing) to billions of entries.
func TestParseBoundsSegmentNumbers(t *testing.T) {
	doc := `<nzb><file subject="x.mkv"><segments>` +
		`<segment number="1">a@b</segment>` +
		`<segment number="2000000000">c@d</segment>` +
		`<segment number="-9000000000000000000">e@f</segment>` +
		`</segments></file></nzb>`
	n, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	f := n.Files[0]
	if got := f.SegmentCount(); got != 1 {
		t.Fatalf("SegmentCount = %d, want 1 (segments %+v)", got, f.Segments)
	}
	if m := f.Missing(); len(m) != 0 {
		t.Fatalf("Missing = %d entries", len(m))
	}
}

func TestRejectsHostilePartCounts(t *testing.T) {
	for _, doc := range []string{
		`<nzb><file subject='"x.mkv" yEnc (1/999999)'><segments><segment bytes="100" number="1">a</segment></segments></file></nzb>`,
		`<nzb><file subject='"x.mkv"'><segments><segment bytes="100" number="999999">a</segment></segments></file></nzb>`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Fatal("accepted hostile sparse file")
		}
	}
}

func TestOldRARThreeDigitVolumes(t *testing.T) {
	var files []*File
	add := func(name string) {
		files = append(files, &File{
			Name:     name,
			Info:     Classify(name),
			Segments: []Segment{{Number: 1, Bytes: 1000, MessageID: name}},
		})
	}
	add("The.Runner.2026.rar")
	for i := 0; i <= 105; i++ {
		if i < 100 {
			add(fmt.Sprintf("The.Runner.2026.r%02d", i))
		} else {
			add(fmt.Sprintf("The.Runner.2026.r%03d", i))
		}
	}
	sets := GroupSets(files)
	if len(sets) != 1 {
		t.Fatalf("got %d sets, want 1", len(sets))
	}
	s := sets[0]
	if s.Kind != KindRAR || len(s.Volumes) != 107 || len(s.Missing) != 0 {
		t.Fatalf("set = kind=%v vols=%d missing=%v", s.Kind, len(s.Volumes), s.Missing)
	}
	if s.Volumes[101].Name != "The.Runner.2026.r100" || s.Volumes[106].Name != "The.Runner.2026.r105" {
		t.Fatalf("tail volumes = %q .. %q", s.Volumes[101].Name, s.Volumes[106].Name)
	}
}

func TestEstimatedDecodedBytes(t *testing.T) {
	empty := &File{}
	if got := empty.EstimatedDecodedBytes(); got != 0 {
		t.Fatalf("empty EstimatedDecodedBytes = %d, want 0", got)
	}
	tiny := &File{Segments: []Segment{{Number: 1, Bytes: 1}}}
	if got := tiny.EstimatedDecodedBytes(); got != 1 {
		t.Fatalf("tiny EstimatedDecodedBytes = %d, want 1", got)
	}
	// Typical 30 MiB FLAC posted as ~3.1% larger yEnc segments (~31.4 MiB encoded).
	const encoded = int64(31_457_280)
	f := &File{Segments: []Segment{
		{Number: 1, Bytes: encoded / 2},
		{Number: 2, Bytes: encoded - encoded/2},
	}}
	est := f.EstimatedDecodedBytes()
	if est <= 0 || est >= f.Bytes() {
		t.Fatalf("EstimatedDecodedBytes = %d, encoded Bytes = %d", est, f.Bytes())
	}
	// ~3.1% overhead removed: est should be roughly 96.5%..97.5% of encoded bytes.
	if est < encoded*965/1000 || est > encoded*975/1000 {
		t.Fatalf("EstimatedDecodedBytes = %d out of expected ~97%% band for %d", est, encoded)
	}
}

func TestSubsetPreservesSubjectNameAcrossReencoding(t *testing.T) {
	raw := []byte(`<nzb>` +
		`<file poster="p" date="1" subject="Other.File.mkv (1/1)"><groups><group>a.b</group></groups><segments><segment bytes="100" number="1">id0@x</segment></segments></file>` +
		`<file poster="p" date="1" subject="Some Show Episode Title 4k.mp4 (1/2)"><groups><group>a.b</group></groups><segments><segment bytes="100" number="1">id1@x</segment></segments></file>` +
		`<file poster="p" date="1" subject="&quot;a1b2c3d4e5f6a1b2c3d4e5f6&quot; yEnc (1/1)"><groups><group>a.b</group></groups><segments><segment bytes="100" number="1">id2@x</segment></segments></file>` +
		`</nzb>`)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Un-renamed NZBs should not emit ether-subject-names metadata.
	plain, ok := doc.Subset(func(*File) bool { return true })
	if !ok {
		t.Fatal("Subset failed")
	}
	if strings.Contains(string(plain), MetaSubjectNames) {
		t.Fatalf("unexpected %s in un-renamed subset:\n%s", MetaSubjectNames, plain)
	}

	// Simulate learned names diverging from original subject names.
	doc.Files[1].Name = "4k.mp4"
	doc.Files[2].Name = "01 - Hidden.flac"
	doc.Files[2].Obfuscated = false

	// Subset dropping index 0 must remap subject-names indices cleanly.
	sub, ok := doc.Subset(func(f *File) bool { return f.Index >= 1 })
	if !ok {
		t.Fatal("Subset failed")
	}
	reparsed, err := Parse(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(reparsed.Files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(reparsed.Files))
	}
	// File 0 (was 1): learned Name="4k.mp4", preserved SubjectName="Some Show Episode Title 4k.mp4".
	if reparsed.Files[0].Name != "4k.mp4" || reparsed.Files[0].OriginalName() != "Some Show Episode Title 4k.mp4" {
		t.Fatalf("file 0 = name=%q orig=%q", reparsed.Files[0].Name, reparsed.Files[0].OriginalName())
	}
	if got := reparsed.Files[0].StableName(); got != "Some Show Episode Title 4k.mp4" {
		t.Fatalf("non-obfuscated StableName = %q, want subject name", got)
	}
	// File 1 (was 2): obfuscated subject -> StableName uses learned name.
	if reparsed.Files[1].Name != "01 - Hidden.flac" || reparsed.Files[1].OriginalName() != "a1b2c3d4e5f6a1b2c3d4e5f6" {
		t.Fatalf("file 1 = name=%q orig=%q", reparsed.Files[1].Name, reparsed.Files[1].OriginalName())
	}
	if got := reparsed.Files[1].StableName(); got != "01 - Hidden.flac" {
		t.Fatalf("obfuscated StableName = %q, want learned name", got)
	}
}

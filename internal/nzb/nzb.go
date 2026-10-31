// Package nzb parses NZB documents: the XML index of a Usenet post that
// lists every file, its newsgroups and the message IDs of its segments
// (articles). Besides the raw structure it derives file names from
// subjects, classifies files (video, archive volumes, par2, extras), flags
// obfuscated names and groups archive volumes into ordered sets, which is
// what the stream engine needs to find the playable file of a release.
package nzb

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInvalid is returned for documents that are not usable NZBs.
var ErrInvalid = errors.New("nzb: invalid document")

// maxDocumentSize bounds what Parse will read (NZBs for 100 GB posts are
// tens of megabytes; anything far larger is not an NZB).
const maxDocumentSize = 256 << 20

// maxSegmentAlternates bounds the repost message-IDs kept per segment, so
// a hostile NZB repeating one number cannot make a miss try thousands.
const maxSegmentAlternates = 4

// maxSegmentNumber bounds a segment's part number. Consumers size per-part
// tables by the highest number (File.SegmentCount), so a hostile
// number="2000000000" must not become a multi-gigabyte allocation. 1<<20
// parts is about 700 GB at typical article sizes.
const maxSegmentNumber = 1 << 20

// NZB is a parsed document.
type NZB struct {
	// Meta holds <head><meta type="..."> values by lower-cased type.
	Meta map[string][]string
	// Files in document order. Files without any valid segment are dropped.
	Files []*File
}

// File is one posted file.
type File struct {
	Index   int // position in NZB.Files
	Poster  string
	Date    time.Time
	Subject string
	Groups  []string
	// Segments sorted by Number, de-duplicated: the first occurrence of a
	// number wins, later message-IDs for it become its Alternates.
	Segments []Segment

	// Name is the file name derived from the subject (may be obfuscated).
	Name string
	// Obfuscated reports that Name looks random (hashes, missing extension),
	// so it says nothing about the content.
	Obfuscated bool
	// Info classifies Name.
	Info FileInfo
	// SubjectIndex / SubjectCount are the "[03/50]" file counter in the
	// subject, 0 when absent. Useful to order obfuscated volumes.
	SubjectIndex int
	SubjectCount int
	// ExpectedSegments is the "(1/N)" segment count in the subject, or 0.
	ExpectedSegments int
	// Duplicates counts repeated segment numbers folded into an earlier
	// segment (as Alternates when the message-ID differs).
	Duplicates int
}

// Segment is one article of a file.
type Segment struct {
	Number    int    // 1-based part number
	Bytes     int64  // article size as listed in the NZB (encoded, approximate)
	MessageID string // without angle brackets
	// Alternates are message-IDs of reposts ("fills") of this same part,
	// in NZB order, to try when MessageID is missing everywhere. A repost
	// carries the same yEnc part, so its bytes must land on the same range.
	Alternates []string
}

// Bytes is the sum of the listed segment sizes. It is the encoded article
// size, a few percent larger than the decoded file; use it for comparisons.
func (f *File) Bytes() int64 {
	var n int64
	for _, s := range f.Segments {
		n += s.Bytes
	}
	return n
}

// SegmentCount is the number of part slots the file has: the highest
// segment number seen or announced by the subject.
func (f *File) SegmentCount() int {
	n := f.ExpectedSegments
	if len(f.Segments) > 0 && f.Segments[len(f.Segments)-1].Number > n {
		n = f.Segments[len(f.Segments)-1].Number
	}
	return n
}

// Missing returns the segment numbers in 1..SegmentCount() that the NZB
// does not list.
func (f *File) Missing() []int {
	total := f.SegmentCount()
	var out []int
	next := 1
	for _, s := range f.Segments {
		for ; next < s.Number && next <= total; next++ {
			out = append(out, next)
		}
		if s.Number >= next {
			next = s.Number + 1
		}
	}
	for ; next <= total; next++ {
		out = append(out, next)
	}
	return out
}

// Password returns the first <meta type="password"> value, if any.
func (n *NZB) Password() string {
	for _, k := range []string{"password", "pass", "pwd"} {
		for _, v := range n.Meta[k] {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}

// MetaValue returns the first meta value of a type ("title", "name", ...).
func (n *NZB) MetaValue(typ string) string {
	if vs := n.Meta[strings.ToLower(typ)]; len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

// TotalBytes sums every file's listed size.
func (n *NZB) TotalBytes() int64 {
	var t int64
	for _, f := range n.Files {
		t += f.Bytes()
	}
	return t
}

// xml structures. Tags carry no namespace, so they match both the
// newzbin-namespaced and the bare form.
type xmlNZB struct {
	Head  xmlHead   `xml:"head"`
	Files []xmlFile `xml:"file"`
}

type xmlHead struct {
	Meta []xmlMeta `xml:"meta"`
}

type xmlMeta struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type xmlFile struct {
	Poster   string       `xml:"poster,attr"`
	Date     string       `xml:"date,attr"`
	Subject  string       `xml:"subject,attr"`
	Groups   []string     `xml:"groups>group"`
	Segments []xmlSegment `xml:"segments>segment"`
}

type xmlSegment struct {
	Bytes  string `xml:"bytes,attr"`
	Number string `xml:"number,attr"`
	ID     string `xml:",chardata"`
}

// Parse parses an NZB document.
func Parse(data []byte) (*NZB, error) {
	return ParseReader(bytes.NewReader(data))
}

// ParseReader parses an NZB document from r.
func ParseReader(r io.Reader) (*NZB, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxDocumentSize))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.CharsetReader = charsetReader
	var doc xmlNZB
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	out := &NZB{Meta: map[string][]string{}}
	for _, m := range doc.Head.Meta {
		t := strings.ToLower(strings.TrimSpace(m.Type))
		if t == "" {
			continue
		}
		out.Meta[t] = append(out.Meta[t], strings.TrimSpace(m.Value))
	}
	for _, xf := range doc.Files {
		f := buildFile(xf)
		if f == nil {
			continue
		}
		f.Index = len(out.Files)
		out.Files = append(out.Files, f)
	}
	if len(out.Files) == 0 {
		return nil, fmt.Errorf("%w: no files with segments", ErrInvalid)
	}
	mergeReposts(out.Files)
	return out, nil
}

func buildFile(xf xmlFile) *File {
	f := &File{
		Poster:  strings.TrimSpace(xf.Poster),
		Subject: strings.TrimSpace(xf.Subject),
	}
	if sec, err := strconv.ParseInt(strings.TrimSpace(xf.Date), 10, 64); err == nil && sec > 0 {
		f.Date = time.Unix(sec, 0).UTC()
	}
	seenGroup := map[string]bool{}
	for _, g := range xf.Groups {
		g = strings.TrimSpace(g)
		if g != "" && !seenGroup[g] {
			seenGroup[g] = true
			f.Groups = append(f.Groups, g)
		}
	}
	segs := make([]Segment, 0, len(xf.Segments))
	for i, xs := range xf.Segments {
		id := strings.TrimSpace(xs.ID)
		id = strings.TrimPrefix(id, "<")
		id = strings.TrimSuffix(id, ">")
		if id == "" {
			continue
		}
		num, err := strconv.Atoi(strings.TrimSpace(xs.Number))
		if err != nil {
			// Some generators omit numbers; fall back to document order.
			num = i + 1
		}
		if num < -maxSegmentNumber || num > maxSegmentNumber {
			continue
		}
		b, _ := strconv.ParseInt(strings.TrimSpace(xs.Bytes), 10, 64)
		if b < 0 {
			b = 0
		}
		segs = append(segs, Segment{Number: num, Bytes: b, MessageID: id})
	}
	if len(segs) == 0 {
		return nil
	}
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].Number < segs[j].Number })
	// A few posters number from 0; shift so parts are 1-based like yEnc.
	if segs[0].Number <= 0 {
		shift := 1 - segs[0].Number
		for i := range segs {
			segs[i].Number += shift
		}
	}
	dedup := segs[:0]
	for _, s := range segs {
		if s.Number > maxSegmentNumber {
			break // sorted: every later number is out of range too
		}
		if len(dedup) > 0 && dedup[len(dedup)-1].Number == s.Number {
			f.Duplicates++
			dedup[len(dedup)-1].addAlternate(s.MessageID)
			continue
		}
		dedup = append(dedup, s)
	}
	f.Segments = dedup

	f.Name = NameFromSubject(f.Subject)
	f.SubjectIndex, f.SubjectCount = subjectFileCounter(f.Subject)
	f.ExpectedSegments = subjectSegmentCount(f.Subject)
	f.Info = Classify(f.Name)
	f.Obfuscated = IsObfuscated(f.Name)
	return f
}

// addAlternate keeps id as a fallback for s unless it is already known.
func (s *Segment) addAlternate(id string) {
	if id == s.MessageID || len(s.Alternates) >= maxSegmentAlternates || slices.Contains(s.Alternates, id) {
		return
	}
	s.Alternates = append(s.Alternates, id)
}

// mergeReposts folds a reposted copy of a whole file into the first copy:
// when two files carry the same (non-obfuscated) name and exactly the same
// segment layout, every part of the later copy becomes an alternate of the
// same part of the first. Both files stay listed (Sets reports the copy in
// Duplicates). Layouts that differ in any part number or listed size are
// left alone: their parts need not cover the same bytes. Obfuscated names
// say nothing about identity, so those copies are not matched.
func mergeReposts(files []*File) {
	first := map[string]*File{}
	for _, f := range files {
		if f.Name == "" || f.Obfuscated {
			continue
		}
		key := strings.ToLower(f.Name)
		orig, ok := first[key]
		if !ok {
			first[key] = f
			continue
		}
		if !sameLayout(orig, f) {
			continue
		}
		for i := range orig.Segments {
			orig.Segments[i].addAlternate(f.Segments[i].MessageID)
			for _, id := range f.Segments[i].Alternates {
				orig.Segments[i].addAlternate(id)
			}
		}
	}
}

func sameLayout(a, b *File) bool {
	if len(a.Segments) != len(b.Segments) || a.SegmentCount() != b.SegmentCount() {
		return false
	}
	for i := range a.Segments {
		if a.Segments[i].Number != b.Segments[i].Number || a.Segments[i].Bytes != b.Segments[i].Bytes || a.Segments[i].Bytes <= 0 {
			return false
		}
	}
	return true
}

// charsetReader handles the encodings NZBs declare in practice.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return input, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "iso-8859-15", "windows-1252", "cp1252":
		b, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		if utf8.Valid(b) {
			// Mislabelled UTF-8 is common; keep it.
			return bytes.NewReader(b), nil
		}
		var sb strings.Builder
		sb.Grow(len(b) + len(b)/8)
		for _, c := range b {
			sb.WriteRune(rune(c))
		}
		return strings.NewReader(sb.String()), nil
	default:
		return nil, fmt.Errorf("unsupported charset %q", label)
	}
}

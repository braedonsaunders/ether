package nntptest

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/yenc"
)

func dial(t *testing.T, s *Server) *textproto.Conn {
	t.Helper()
	var nc net.Conn
	var err error
	if s.ClientTLSConfig() != nil {
		nc, err = tls.Dial("tcp", s.Addr(), s.ClientTLSConfig())
	} else {
		nc, err = net.Dial("tcp", s.Addr())
	}
	if err != nil {
		t.Fatal(err)
	}
	c := textproto.NewConn(nc)
	t.Cleanup(func() { c.Close() })
	if _, _, err := c.ReadCodeLine(200); err != nil {
		t.Fatal(err)
	}
	return c
}

func cmd(t *testing.T, c *textproto.Conn, want int, format string, args ...any) string {
	t.Helper()
	id, err := c.Cmd(format, args...)
	if err != nil {
		t.Fatal(err)
	}
	c.StartResponse(id)
	defer c.EndResponse(id)
	_, msg, err := c.ReadCodeLine(want)
	if err != nil {
		t.Fatalf("%s: %v", format, err)
	}
	return msg
}

func fetchBody(t *testing.T, c *textproto.Conn, id string) []byte {
	t.Helper()
	cmd(t, c, 222, "BODY <%s>", id)
	b, err := io.ReadAll(c.DotReader())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type nzbDoc struct {
	XMLName xml.Name `xml:"nzb"`
	Head    []struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"head>meta"`
	Files []struct {
		Poster   string   `xml:"poster,attr"`
		Date     int64    `xml:"date,attr"`
		Subject  string   `xml:"subject,attr"`
		Groups   []string `xml:"groups>group"`
		Segments []struct {
			Bytes  int    `xml:"bytes,attr"`
			Number int    `xml:"number,attr"`
			ID     string `xml:",chardata"`
		} `xml:"segments>segment"`
	} `xml:"file"`
}

func TestPostAndFetch(t *testing.T) {
	s := Start(t, Options{})
	r := rand.New(rand.NewSource(1))
	a := make([]byte, 250_000)
	r.Read(a)
	b := []byte("tiny & <odd> \"name\" file")
	post, err := s.Post(map[string][]byte{"b.nfo": b, "a.mkv": a}, 100_000, PostOptions{Meta: map[string]string{"title": "T & U"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc nzbDoc
	if err := xml.Unmarshal(post.NZB, &doc); err != nil {
		t.Fatalf("nzb does not parse: %v\n%s", err, post.NZB)
	}
	if doc.XMLName.Space != "http://www.newzbin.com/DTD/2003/nzb" || !bytes.Contains(post.NZB, []byte("<!DOCTYPE nzb")) {
		t.Fatal("missing namespace or doctype")
	}
	if len(doc.Head) != 1 || doc.Head[0].Type != "title" || doc.Head[0].Value != "T & U" {
		t.Fatalf("head %+v", doc.Head)
	}
	if len(doc.Files) != 2 || len(doc.Files[0].Segments) != 3 || len(doc.Files[1].Segments) != 1 {
		t.Fatalf("files %+v", doc.Files)
	}
	f0 := doc.Files[0]
	if !strings.Contains(f0.Subject, `"a.mkv"`) || f0.Poster == "" || f0.Date != 1704067200 || f0.Groups[0] != "alt.binaries.test" {
		t.Fatalf("file attrs %+v", f0)
	}

	c := dial(t, s)
	var got []byte
	for i, sg := range f0.Segments {
		if sg.Number != i+1 || sg.Bytes <= 0 {
			t.Fatalf("segment %+v", sg)
		}
		body := fetchBody(t, c, sg.ID)
		p, err := yenc.Decode(body)
		if err != nil {
			t.Fatalf("segment %d: %v", i+1, err)
		}
		if p.Header.Part != i+1 || p.Header.Total != 3 || p.Header.Begin != int64(i*100_000+1) || p.Header.Size != 250_000 {
			t.Fatalf("header %+v", p.Header)
		}
		got = append(got, p.Data...)
	}
	if !bytes.Equal(got, a) {
		t.Fatal("reassembled mismatch")
	}
	if s.BodyRequests(f0.Segments[0].ID) != 1 {
		t.Fatal("body request not counted")
	}
	if post.Files[0].Segments[2].End != 250_000 || post.Files[1].Name != "b.nfo" {
		t.Fatalf("posting %+v", post.Files)
	}
}

func TestDotStuffingAndCommands(t *testing.T) {
	s := Start(t, Options{})
	s.AddArticle("x@y", []byte(".leading dot\n..two\nplain\r\n."))
	c := dial(t, s)
	body := fetchBody(t, c, "x@y")
	if string(body) != ".leading dot\n..two\nplain\n.\n" {
		t.Fatalf("body %q", body)
	}
	cmd(t, c, 223, "STAT <x@y>")
	cmd(t, c, 430, "STAT <nope@y>")
	cmd(t, c, 211, "GROUP alt.binaries.test")
	if msg := cmd(t, c, 111, "DATE"); len(msg) != 14 {
		t.Fatalf("date %q", msg)
	}
	cmd(t, c, 500, "FROB")
	cmd(t, c, 501, "BODY x@y")
	s.SetMissing(true, "x@y")
	cmd(t, c, 430, "BODY <x@y>")
	if s.Has("x@y") {
		t.Fatal("Has should honour missing")
	}
	s.SetMissing(false, "<x@y>")
	cmd(t, c, 223, "STAT <x@y>")
	s.Remove("x@y")
	cmd(t, c, 430, "STAT <x@y>")
	cmd(t, c, 205, "QUIT")
	st := s.Stats()
	if st.Commands["STAT"] != 4 || st.ArticleMisses != 3 {
		t.Fatalf("stats %+v", st)
	}
}

func TestAuth(t *testing.T) {
	s := Start(t, Options{Username: "u", Password: "p"})
	s.AddArticle("a@b", []byte("hi"))
	c := dial(t, s)
	cmd(t, c, 480, "BODY <a@b>")
	cmd(t, c, 482, "AUTHINFO PASS p")
	cmd(t, c, 381, "AUTHINFO USER u")
	cmd(t, c, 481, "AUTHINFO PASS wrong")
	cmd(t, c, 381, "AUTHINFO USER u")
	cmd(t, c, 281, "AUTHINFO PASS p")
	if got := fetchBody(t, c, "a@b"); string(got) != "hi\n" {
		t.Fatalf("got %q", got)
	}
	if s.Stats().AuthFailures != 1 {
		t.Fatal("auth failure not counted")
	}
	p := s.Provider("x", 3)
	if p.Username != "u" || p.Password != "p" || p.Connections != 3 || !p.Enabled || p.Port != s.Port() {
		t.Fatalf("provider %+v", p)
	}
}

func TestTLS(t *testing.T) {
	s := Start(t, Options{TLS: true})
	s.AddArticle("a@b", []byte("secure"))
	c := dial(t, s)
	if got := fetchBody(t, c, "a@b"); string(got) != "secure\n" {
		t.Fatalf("got %q", got)
	}
	if !s.Provider("x", 1).TLS {
		t.Fatal("provider TLS flag")
	}
}

func TestMaxConnsAndDrop(t *testing.T) {
	s := Start(t, Options{MaxConns: 1})
	c := dial(t, s)
	nc, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	line, _ := textproto.NewReader(bufio.NewReader(nc)).ReadLine()
	if !strings.HasPrefix(line, "502") {
		t.Fatalf("second connection got %q", line)
	}
	st := s.Stats()
	if st.Rejected != 1 || st.MaxActive != 1 {
		t.Fatalf("stats %+v", st)
	}
	s.DropConnections()
	if _, err := c.Cmd("DATE"); err == nil {
		if _, _, err := c.ReadCodeLine(111); err == nil {
			t.Fatal("connection should be dropped")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Stats().Active != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Stats().Active != 0 {
		t.Fatal("active not released")
	}
}

func TestRefusalTextAndTeardownDelay(t *testing.T) {
	s := Start(t, Options{MaxConns: 1, RefuseCode: 481, RefuseText: "connection limit (1) reached", TeardownDelay: 100 * time.Millisecond})
	first := func() string {
		nc, err := net.Dial("tcp", s.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		line, _ := textproto.NewReader(bufio.NewReader(nc)).ReadLine()
		return line
	}
	c := dial(t, s)
	if line := first(); line != "481 connection limit (1) reached" {
		t.Fatalf("refusal %q", line)
	}
	c.Close()
	// The closed connection still counts until the teardown delay passes.
	time.Sleep(20 * time.Millisecond)
	if line := first(); !strings.HasPrefix(line, "481") {
		t.Fatalf("redial during teardown got %q", line)
	}
	time.Sleep(150 * time.Millisecond)
	if line := first(); !strings.HasPrefix(line, "200") {
		t.Fatalf("redial after teardown got %q", line)
	}
}

func TestFaultsAndCorrupt(t *testing.T) {
	s := Start(t, Options{})
	data := bytes.Repeat([]byte("0123456789"), 1000)
	if _, err := s.AddYenc("y@z", data, yenc.EncodeOptions{Name: "f"}); err != nil {
		t.Fatal(err)
	}
	n := 0
	s.SetFault(func(cmd, arg string) Fault {
		if cmd == "BODY" && arg == "y@z" {
			n++
			if n == 1 {
				return Fault{Code: 503, Text: "busy"}
			}
		}
		return Fault{}
	})
	c := dial(t, s)
	cmd(t, c, 503, "BODY <y@z>")
	p, err := yenc.Decode(fetchBody(t, c, "y@z"))
	if err != nil || !bytes.Equal(p.Data, data) {
		t.Fatalf("decode: %v", err)
	}
	if err := s.Corrupt("y@z"); err != nil {
		t.Fatal(err)
	}
	if _, err := yenc.Decode(fetchBody(t, c, "y@z")); !errors.Is(err, yenc.ErrCRCMismatch) {
		t.Fatalf("err = %v", err)
	}
	if err := s.Corrupt("missing@z"); err == nil {
		t.Fatal("expected error")
	}
}

// Real clients may enqueue another BODY while the preceding body's network
// response is delayed. The stall fault must preserve that command byte-for-byte.
func TestBodyDelayPreservesCommandSentMidBody(t *testing.T) {
	s := Start(t, Options{})
	if _, err := s.AddYenc("first@test", []byte("first"), yenc.EncodeOptions{Name: "one.bin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddYenc("second@test", []byte("second"), yenc.EncodeOptions{Name: "two.bin"}); err != nil {
		t.Fatal(err)
	}
	s.SetFault(func(cmd, id string) Fault {
		if cmd == "BODY" && id == "first@test" {
			return Fault{BodyDelay: 30 * time.Millisecond}
		}
		return Fault{}
	})
	c := dial(t, s)
	defer c.Close()
	if err := c.PrintfLine("BODY <first@test>"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.ReadResponse(222); err != nil {
		t.Fatal(err)
	}
	if line, err := c.ReadLine(); err != nil || !strings.HasPrefix(line, "=ybegin ") {
		t.Fatalf("first header %q %v", line, err)
	}
	// This arrives after the fake server has entered its delay.
	if err := c.PrintfLine("BODY <second@test>"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadDotBytes(); err != nil {
		t.Fatalf("first remainder: %v", err)
	}
	if _, _, err := c.ReadResponse(222); err != nil {
		t.Fatalf("queued BODY corrupted: %v", err)
	}
	body, err := c.ReadDotBytes()
	if err != nil {
		t.Fatal(err)
	}
	part, err := yenc.Decode(body)
	if err != nil || string(part.Data) != "second" {
		t.Fatalf("second payload %v %v", part, err)
	}
}

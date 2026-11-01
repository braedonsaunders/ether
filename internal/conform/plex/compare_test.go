package plex

import (
	"net/http"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
)

func xmlResp(body string) conform.Response {
	return conform.Response{Status: 200, Headers: http.Header{"Content-Type": []string{"text/xml;charset=utf-8"}}, Body: []byte(body)}
}

func jsonResp(body string) conform.Response {
	return conform.Response{Status: 200, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(body)}
}

func findDiff(r conform.Report, kind, path string) *conform.Difference {
	for i := range r.Differences {
		if r.Differences[i].Kind == kind && r.Differences[i].Path == path {
			return &r.Differences[i]
		}
	}
	return nil
}

func TestPlexCompareLoadBearingBlocks(t *testing.T) {
	ref := xmlResp(`<MediaContainer size="1" allowSync="1" viewGroup="movie"><Hub key="/hubs/sections/1/x" hubIdentifier="movie.recent" style="shelf" type="movie" more="0"/></MediaContainer>`)
	tgt := xmlResp(`<MediaContainer size="2" allowSync="0" viewGroup="show"><Hub key="/hubs/items?identifier=x" hubIdentifier="movie.recent" style="hero" type="show" more="1"/></MediaContainer>`)
	r := Compare(ref, tgt)
	for _, path := range []string{
		"/MediaContainer/@size",
		"/MediaContainer/@allowSync",
		"/MediaContainer/@viewGroup",
		"/MediaContainer/Hub/@style",
		"/MediaContainer/Hub/@type",
		"/MediaContainer/Hub/@more",
		"/MediaContainer/Hub/@key",
	} {
		d := findDiff(r, conform.KindValue, path)
		if d == nil || !d.Blocking {
			t.Fatalf("%s should be a blocking Plex load-bearing value: %+v", path, r.Differences)
		}
	}
}

func TestPlexCompareMultiValueHubsBlock(t *testing.T) {
	ref := xmlResp(`<MediaContainer size="3">
		<Hub hubIdentifier="movie.recent" style="shelf" more="0" type="movie"/>
		<Hub hubIdentifier="movie.top" style="hero" more="1" type="movie"/>
		<Hub hubIdentifier="tv.recent" style="shelf" more="0" type="show"/>
	</MediaContainer>`)
	tgt := xmlResp(`<MediaContainer size="3">
		<Hub hubIdentifier="wrong.a" style="bogus" more="1" type="clip"/>
		<Hub hubIdentifier="wrong.b" style="bogus" more="1" type="clip"/>
		<Hub hubIdentifier="wrong.c" style="bogus" more="1" type="clip"/>
	</MediaContainer>`)
	r := Compare(ref, tgt)
	for _, path := range []string{
		"/MediaContainer/Hub/@hubIdentifier",
		"/MediaContainer/Hub/@style",
		"/MediaContainer/Hub/@more",
		"/MediaContainer/Hub/@type",
	} {
		d := findDiff(r, conform.KindValue, path)
		if d == nil || !d.Blocking {
			t.Fatalf("%s must block on mixed hub values: %+v", path, r.Differences)
		}
	}
}

func TestPlexCompareHubOrderBlocks(t *testing.T) {
	ref := xmlResp(`<MediaContainer>
		<Hub hubIdentifier="a.one" style="shelf"/>
		<Hub hubIdentifier="b.two" style="shelf"/>
		<Hub hubIdentifier="c.three" style="shelf"/>
	</MediaContainer>`)
	tgt := xmlResp(`<MediaContainer>
		<Hub hubIdentifier="a.one" style="shelf"/>
		<Hub hubIdentifier="c.three" style="shelf"/>
		<Hub hubIdentifier="b.two" style="shelf"/>
	</MediaContainer>`)
	r := Compare(ref, tgt)
	d := findDiff(r, conform.KindValue, "/MediaContainer/Hub/#order")
	if d == nil || !d.Blocking || !strings.Contains(d.Reference.(string), "b.two") {
		t.Fatalf("hub #order must block: %+v", r.Differences)
	}
	same := Compare(ref, ref)
	if findDiff(same, conform.KindValue, "/MediaContainer/Hub/#order") != nil {
		t.Fatal("matching hub order must not differ")
	}
}

func TestPlexCompareSharedCompareDoesNotBlockSiloType(t *testing.T) {
	// Shared Compare must keep KindValue informational so Silo's lowercase
	// type is not a Plex load-bearing failure.
	ref := jsonResp(`{"Type":"Movie","Name":"A"}`)
	tgt := jsonResp(`{"Type":"movie","Name":"A"}`)
	r := conform.Compare(ref, tgt)
	d := findDiff(r, conform.KindValue, "$/Type")
	if d != nil && d.Blocking {
		t.Fatalf("shared Compare must not block type: %+v", r.Differences)
	}
}

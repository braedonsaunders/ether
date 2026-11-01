package conform

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func xmlResp(body string) Response {
	return Response{Status: 200, Headers: http.Header{"Content-Type": []string{"text/xml;charset=utf-8"}}, Body: []byte(body)}
}

func jsonResp(body string) Response {
	return Response{Status: 200, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(body)}
}

func find(r Report, kind, path string) *Difference {
	for i := range r.Differences {
		if r.Differences[i].Kind == kind && r.Differences[i].Path == path {
			return &r.Differences[i]
		}
	}
	return nil
}

func TestCompareCollapsesArrayIndices(t *testing.T) {
	ref := xmlResp(`<MediaContainer size="3">
		<Video ratingKey="1" title="A" slug="a"><Image type="coverPoster" url="/library/metadata/1/thumb/9"/></Video>
		<Video ratingKey="2" title="B" slug="b"><Image type="coverPoster" url="/library/metadata/2/thumb/9"/></Video>
		<Video ratingKey="3" title="C"/>
	</MediaContainer>`)
	tgt := xmlResp(`<MediaContainer size="2">
		<Video ratingKey="10000" title="Other" extra="1"/>
		<Video ratingKey="10001" title="Again"/>
	</MediaContainer>`)
	r := Compare(ref, tgt)
	if d := find(r, KindMissing, "/MediaContainer/Video/@slug"); d == nil || d.Count != 2 {
		t.Fatalf("slug missing once with count 2: %+v", r.Differences)
	}
	img := find(r, KindMissing, "/MediaContainer/Video/Image")
	if img == nil || img.Nested != 2 || img.Count != 2 {
		t.Fatalf("Image missing folded with 2 nested attrs: %+v", r.Differences)
	}
	if find(r, KindMissing, "/MediaContainer/Video/Image/@url") != nil {
		t.Fatal("nested paths of a missing element must be folded")
	}
	if d := find(r, KindExtra, "/MediaContainer/Video/@extra"); d == nil || !d.Blocking {
		t.Fatalf("extra attr: %+v", r.Differences)
	}
	// Titles stay volatile. Container size is load-bearing.
	if d := find(r, KindValue, "/MediaContainer/@size"); d == nil || !d.Blocking || d.Reference != "3" || d.Target != "2" {
		t.Fatalf("size must be a blocking load-bearing diff: %+v", r.Differences)
	}
	for _, d := range r.Differences {
		if d.Kind == KindValue && d.Path != "/MediaContainer/@size" {
			t.Fatalf("unexpected value diff %+v", d)
		}
	}
	if len(r.Differences) != 4 {
		t.Fatalf("want 4 differences, got %+v", r.Differences)
	}
}

func TestCompareJSONTypesAndContainers(t *testing.T) {
	ref := jsonResp(`{"MediaContainer":{"size":2,"Metadata":[
		{"ratingKey":"1","librarySectionID":"1","UltraBlurColors":{"topLeft":"112233"},"Genre":[{"tag":"Drama"}]},
		{"ratingKey":"2","librarySectionID":"1","UltraBlurColors":{"topLeft":"445566"}}]}}`)
	tgt := jsonResp(`{"MediaContainer":{"size":1,"Metadata":[
		{"ratingKey":"9","librarySectionID":1,"UltraBlurColors":[{"topLeft":"778899"}],"Genre":[{"tag":"Comedy"}]}]}}`)
	r := Compare(ref, tgt)
	if d := find(r, KindType, "$/MediaContainer/Metadata[]/librarySectionID"); d == nil || d.Reference != "string" || d.Target != "number" {
		t.Fatalf("librarySectionID type: %+v", r.Differences)
	}
	if d := find(r, KindType, "$/MediaContainer/Metadata[]/UltraBlurColors"); d == nil || d.Reference != "object" || d.Target != "array" {
		t.Fatalf("object vs array: %+v", r.Differences)
	}
	if find(r, KindValue, "$/MediaContainer/Metadata[]/Genre[]/tag") != nil {
		t.Fatal("tags are volatile")
	}
}

func TestCompareXMLFreeTextAndRedacted(t *testing.T) {
	ref := xmlResp(`<MediaContainer size="1" multiuser="[REDACTED]" platform="Windows"><Video title="1917" videoResolution="1080"/></MediaContainer>`)
	tgt := xmlResp(`<MediaContainer size="1" multiuser="1" platform="MacOSX"><Video title="Alien" videoResolution="4k"/></MediaContainer>`)
	r := Compare(ref, tgt)
	if len(r.Differences) != 0 {
		t.Fatalf("free text, redacted and volatile values must not differ: %+v", r.Differences)
	}
}

func TestCompareLoadBearingValuesBlock(t *testing.T) {
	ref := xmlResp(`<MediaContainer size="1" allowSync="1" viewGroup="movie" viewMode="65592" nocache="1"><Hub key="/hubs/sections/1/x" hubIdentifier="movie.recent" context="hub.movie.recent" style="shelf" type="movie" more="0"/></MediaContainer>`)
	tgt := xmlResp(`<MediaContainer size="2" allowSync="0" viewGroup="show" viewMode="65593" nocache="0"><Hub key="/hubs/items?identifier=x" hubIdentifier="movie.recent" context="hub.movie.recent" style="hero" type="show" more="1"/></MediaContainer>`)
	r := Compare(ref, tgt)
	for _, path := range []string{
		"/MediaContainer/@size",
		"/MediaContainer/@allowSync",
		"/MediaContainer/@viewGroup",
		"/MediaContainer/@viewMode",
		"/MediaContainer/@nocache",
		"/MediaContainer/Hub/@style",
		"/MediaContainer/Hub/@type",
		"/MediaContainer/Hub/@more",
		"/MediaContainer/Hub/@key",
	} {
		d := find(r, KindValue, path)
		if d == nil || !d.Blocking {
			t.Fatalf("%s should be a blocking load-bearing value: %+v", path, r.Differences)
		}
	}
	if d := find(r, KindValue, "/MediaContainer/Hub/@key"); d.Reference != "/hubs/sections/{id}/x" {
		t.Fatalf("key shapes compared: %+v", r.Differences)
	}
	if r.Blocking() < 9 {
		t.Fatalf("load-bearing mismatches must fail CI: %+v", r.Differences)
	}
	same := xmlResp(`<MediaContainer size="1" allowSync="1" viewGroup="movie"><Hub key="/hubs/sections/9/x" ratingKey="42"/></MediaContainer>`)
	other := xmlResp(`<MediaContainer size="1" allowSync="1" viewGroup="movie"><Hub key="/hubs/sections/1/x" ratingKey="100"/></MediaContainer>`)
	if n := Compare(same, other).Blocking(); n != 0 {
		t.Fatalf("matching key/ratingKey shapes must not block: %+v", Compare(same, other).Differences)
	}
	// A JSON boolean where PMS sends 0/1 is a type mismatch.
	r = Compare(jsonResp(`{"MediaContainer":{"allowSync":1}}`), jsonResp(`{"MediaContainer":{"allowSync":true}}`))
	if d := find(r, KindType, "$/MediaContainer/allowSync"); d == nil {
		t.Fatalf("number vs boolean: %+v", r.Differences)
	}
}

func TestCompareChildOrder(t *testing.T) {
	ref := xmlResp(`<MediaContainer><Video><Image/><UltraBlurColors/><Genre/></Video><Video><Image/><Genre/></Video></MediaContainer>`)
	tgt := xmlResp(`<MediaContainer><Video><Genre/><Image/><UltraBlurColors/></Video></MediaContainer>`)
	r := Compare(ref, tgt)
	d := find(r, KindValue, "/MediaContainer/Video/#order")
	if d == nil || !strings.Contains(d.Reference.(string), "Image before Genre") {
		t.Fatalf("order diff: %+v", r.Differences)
	}
	if d.Blocking {
		t.Fatal("order is informational")
	}
}

func TestCompareRepairsBareRedactionsInJSON(t *testing.T) {
	ref := jsonResp(`{"MediaContainer":{"size":0,"multiuser":[REDACTED],"version":"1"}}`)
	tgt := jsonResp(`{"MediaContainer":{"size":0,"multiuser":true,"version":"2"}}`)
	r := Compare(ref, tgt)
	if len(r.Differences) != 0 {
		t.Fatalf("redacted fixture should parse and compare: %+v", r.Differences)
	}
	// The old pattern also swallowed the comma after the value.
	swallowed := jsonResp(`{"MediaContainer":{"size":0,"multiuser":[REDACTED]"version":"1"}}`)
	if r := Compare(swallowed, tgt); len(r.Differences) != 0 {
		t.Fatalf("swallowed comma should be repaired: %+v", r.Differences)
	}
	r = Compare(ref, jsonResp(`not json`))
	if find(r, KindParse, "$body") == nil {
		t.Fatalf("parse error expected: %+v", r.Differences)
	}
}

func TestCompareStatusAndHeaders(t *testing.T) {
	ref := xmlResp(`<MediaContainer/>`)
	tgt := Response{Status: 404, Headers: http.Header{"Content-Type": []string{"text/html"}}, Body: []byte(`<html><body>404</body></html>`)}
	r := Compare(ref, tgt)
	for _, want := range []string{KindStatus, KindHeader} {
		found := false
		for _, d := range r.Differences {
			found = found || (d.Kind == want && d.Blocking)
		}
		if !found {
			t.Errorf("missing %s difference: %+v", want, r.Differences)
		}
	}
}

func TestSummarizeGroupsByEndpointAcrossClients(t *testing.T) {
	ref := xmlResp(`<MediaContainer><Video slug="a"/></MediaContainer>`)
	tgt := xmlResp(`<MediaContainer><Video/></MediaContainer>`)
	var rep Report
	rep.Endpoints = []string{"/library/sections/{{section}}/all", "/identity"}
	for _, c := range []string{"web", "ios", "appletv"} {
		r := Compare(ref, tgt).WithRequest(c, "xml", "/library/sections/{{section}}/all")
		rep.Differences = append(rep.Differences, r.Differences...)
	}
	sums := Summarize(rep)
	if len(sums) != 2 {
		t.Fatalf("want 2 endpoints (one clean), got %+v", sums)
	}
	var all EndpointSummary
	for _, s := range sums {
		if s.Endpoint == "/library/sections/{{section}}/all" {
			all = s
		}
	}
	if len(all.Missing) != 1 || strings.Join(all.Missing[0].Clients, ",") != "appletv,ios,web" {
		t.Fatalf("missing slug grouped across clients: %+v", all)
	}
	var buf bytes.Buffer
	WriteSummary(&buf, rep, false)
	out := buf.String()
	for _, want := range []string{"== /library/sections/{{section}}/all", "missing (1)", "/MediaContainer/Video/@slug", "[appletv,ios,web]", "| `/identity` | 0 | 0 | 0 | 0 | 0 |", "| **total** | 0 | 1 | 0 | 0 | 0 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	buf.Reset()
	WriteText(&buf, rep, false)
	if !strings.Contains(buf.String(), "== ios xml /library/sections/{{section}}/all") {
		t.Errorf("text report groups by request:\n%s", buf.String())
	}
}

func TestRedactKeepsJSONValid(t *testing.T) {
	in := `{"MediaContainer":{"multiuser":true,"friendlyName":"home","myPlexUsername":"bob"}}`
	out := string(Redact([]byte(in)))
	if strings.Contains(out, "home") || strings.Contains(out, "bob") {
		t.Fatalf("not redacted: %s", out)
	}
	if !strings.Contains(out, `"multiuser":true`) {
		t.Fatalf("multiuser must not be redacted: %s", out)
	}
	if _, err := decodeJSON([]byte(out)); err != nil {
		t.Fatalf("redaction broke JSON: %v (%s)", err, out)
	}
}

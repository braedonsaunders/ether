package plex

import (
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/plex/model"
)

// TestParseRequestHeaderPlatformBeatsQueryGeneric proves the real wire
// protection for Plezy on Android/tvOS: plexParam reads the header before
// the query, so a decision request whose query says X-Plex-Platform=Generic
// still yields the platform caps from the header, not htpcCaps/genericCaps.
func TestParseRequestHeaderPlatformBeatsQueryGeneric(t *testing.T) {
	cases := []struct {
		name      string
		platform  string // X-Plex-Platform header
		device    string // X-Plex-Device header
		maxHeight int
		hdr, dv   bool
	}{
		{"plezy android phone", "Android", "Pixel 8", 1080, false, false}, // androidCaps
		{"plezy apple tv", "tvOS", "Apple TV", 2160, true, true},          // appleTVCaps
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/video/:/transcode/universal/decision?X-Plex-Platform=Generic", nil)
			r.Header.Set("X-Plex-Product", "Plezy")
			r.Header.Set("X-Plex-Platform", tc.platform)
			r.Header.Set("X-Plex-Device", tc.device)
			pr := ParseRequest(r)
			if pr.Platform != tc.platform {
				t.Fatalf("Platform = %q, want header %q to beat query Generic", pr.Platform, tc.platform)
			}
			caps := pr.ClientCaps(0)
			if caps.MaxHeight != tc.maxHeight || caps.HDR10 != tc.hdr || caps.DolbyVision != tc.dv {
				t.Fatalf("caps = %+v, want maxHeight=%d hdr=%v dv=%v", caps, tc.maxHeight, tc.hdr, tc.dv)
			}
		})
	}
}

func TestParseRequestNegotiatesFormatFromAccept(t *testing.T) {
	cases := []struct {
		name   string
		accept []string
		want   model.Format
	}{
		{"json only", []string{"application/json"}, model.JSON},
		{"plexapi prefers xml", []string{"application/xml, application/json;q=0.9,*/*;q=0.5"}, model.XML},
		{"plex web", []string{"application/json, text/plain, */*"}, model.JSON},
		{"xml only", []string{"application/xml"}, model.XML},
		{"no accept", nil, model.XML},
		{"wildcard", []string{"*/*"}, model.XML},
		{"json lower q", []string{"application/json;q=0.5, application/xml"}, model.XML},
		{"json higher q", []string{"application/xml;q=0.8, application/json"}, model.JSON},
		{"equal q json first", []string{"application/json, application/xml"}, model.JSON},
		{"equal q xml first", []string{"text/xml, application/json"}, model.XML},
		{"json refused", []string{"application/json;q=0"}, model.XML},
		{"json with charset", []string{"application/json; charset=utf-8"}, model.JSON},
		{"split headers", []string{"application/xml", "application/json;q=0.9"}, model.XML},
		{"split tie xml first", []string{"text/xml", "application/json"}, model.XML},
		{"split tie json first", []string{"application/json", "text/xml"}, model.JSON},
		{"xml refused", []string{"application/xml;q=0, application/json;q=0.2"}, model.JSON},
		{"both refused", []string{"application/json;q=0, application/xml;q=0"}, model.XML},
		{"invalid json quality", []string{"application/json;q=bogus, application/xml"}, model.XML},
		{"negative json quality", []string{"application/json;q=-1, application/xml"}, model.XML},
		{"wildcard with explicit json", []string{"*/*, application/json"}, model.JSON},
		{"wildcard with refused json", []string{"*/*, application/json;q=0"}, model.XML},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/identity", nil)
			for _, a := range tc.accept {
				r.Header.Add("Accept", a)
			}
			if got := ParseRequest(r).Format; got != tc.want {
				t.Fatalf("Accept %q: format %v, want %v", tc.accept, got, tc.want)
			}
		})
	}
}

package plex

import (
	"path/filepath"
	"strings"
	"testing"

	pmsconform "github.com/braedonsaunders/ether/internal/conform"
	conformplex "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/core"
)

// TestRecordedHubsSearchSectionsLoadBearing compares Ether to the PMS
// recordings for hubs, search and sections on load-bearing fields.
func TestRecordedHubsSearchSectionsLoadBearing(t *testing.T) {
	for _, probe := range conformplex.Corpus() {
		if !browseLoadBearingPath(probe.Path) {
			continue
		}
		for profile, client := range conformplex.Clients() {
			for _, jsonMode := range []bool{false, true} {
				t.Run(profile+"/"+pmsFixtureName(profile, probe.Path, jsonMode), func(t *testing.T) {
					ref, err := pmsconform.LoadFixture(filepath.Join("..", "..", "testdata", "conform", "plex", profile, pmsFixtureName(profile, probe.Path, jsonMode)))
					if err != nil {
						t.Fatal(err)
					}
					f := newFixture()
					f.srv.d.Identity.PlatformVersion = "6.1.0"
					defer f.srv.Close()
					for _, title := range []core.Title{burbs, nineShow, nineS1, nineE1} {
						f.catalog.titles[title.Key] = title
					}
					f.catalog.children[nineShow.Key] = []core.RatingKey{nineS1.Key}
					f.catalog.children[nineS1.Key] = []core.RatingKey{nineE1.Key}
					f.playback.info = burbsInfo
					target := strings.NewReplacer("{{section}}", "1", "{{movieSection}}", "1", "{{showSection}}", "2", "{{movieKey}}", burbs.Key.String(), "{{showKey}}", nineShow.Key.String(), "{{seasonKey}}", nineS1.Key.String(), "{{episodeKey}}", nineE1.Key.String(), "{{collectionKey}}", "1", "{{playlistKey}}", "1").Replace(probe.Path)
					hdr := map[string]string{}
					for k, v := range client.Headers {
						if !strings.EqualFold(k, "X-Plex-Token") {
							hdr[k] = v[0]
						}
					}
					hdr["Accept"] = "application/xml"
					if jsonMode {
						hdr["Accept"] = "application/json"
					}
					rec := f.request(t, probe.Method, target, hdr)
					if rec.Code != ref.Status {
						t.Fatalf("status %d want %d: %s", rec.Code, ref.Status, rec.Body.String())
					}
					ours := pmsconform.Response{Status: rec.Code, Headers: rec.Header(), Body: rec.Body.Bytes()}
					for _, d := range conformplex.Compare(ref, ours).Differences {
						if !d.Blocking || d.Kind != pmsconform.KindValue {
							continue
						}
						if skip, reason := knownLoadBearingGap(probe.Path, d); skip {
							t.Logf("skip %s %s: %s", d.Kind, d.Path, reason)
							continue
						}
						t.Errorf("%s %s PMS=%v ours=%v", d.Kind, d.Path, d.Reference, d.Target)
					}
				})
			}
		}
	}
}

func browseLoadBearingPath(path string) bool {
	return strings.HasPrefix(path, "/hubs") || strings.HasPrefix(path, "/library/search") || strings.HasPrefix(path, "/library/sections")
}

func knownLoadBearingGap(endpoint string, d pmsconform.Difference) (bool, string) {
	leaf := d.Path
	if i := strings.LastIndex(leaf, "/"); i >= 0 {
		leaf = leaf[i+1:]
	}
	leaf = strings.TrimPrefix(leaf, "@")
	leaf = strings.TrimSuffix(leaf, "[]")
	switch leaf {
	case "size", "totalSize", "leafCount", "viewedLeafCount", "childCount":
		return true, "catalog counts differ from the recorded PMS library; slices B (browse), C (search hubs) and D (dashboard) own the content"
	case "librarySectionID":
		return true, "section IDs are assigned per server; this fixture uses 1/2"
	case "hubIdentifier", "context", "style", "more":
		if strings.HasPrefix(endpoint, "/hubs") || strings.HasPrefix(endpoint, "/library/search") {
			return true, "hub composition differs from the recorded PMS home/search; slice C owns search hubs, slice D owns the dashboard"
		}
	case "type":
		if strings.HasPrefix(endpoint, "/hubs") || strings.HasPrefix(endpoint, "/library/search") {
			return true, "hub row types follow hub composition owned by slices C and D"
		}
	case "allowSync":
		return true, "Ether answers allowSync=0 on these listings; PMS 1.43.4 answers 1. Changing Plex production responses is out of this slice"
	}
	return false, ""
}

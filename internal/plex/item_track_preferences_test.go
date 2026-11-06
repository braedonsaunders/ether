package plex

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestItemTrackPreferencesInheritance(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	for _, tc := range []struct {
		key     core.RatingKey
		inherit string
	}{
		{core.ShowKey(1396), "Account default"},
		{core.SeasonKey(1396, 1), "Series default"},
		{fightClub.Key, ""},
		{core.EpisodeKey(1396, 1, 1), ""},
	} {
		path := "/library/metadata/" + tc.key.String()
		for _, flag := range []string{"", "?includePreferences=01", "?includePreferences=1"} {
			rec := f.get(t, path+flag)
			if rec.Code != 200 {
				t.Fatalf("%s%s: %d", path, flag, rec.Code)
			}
			body := rec.Body.String()
			want := flag == "?includePreferences=1" && tc.inherit != ""
			if strings.Contains(body, "<Preferences>") != want {
				t.Fatalf("%s%s preferences: %s", path, flag, body)
			}
			if want && (!strings.Contains(body, `id="audioLanguage"`) || !strings.Contains(body, `enumValues=":`+tc.inherit+`|`) || !strings.Contains(body, `id="subtitleLanguage"`)) {
				t.Fatalf("inheritance: %s", body)
			}
		}
	}
}

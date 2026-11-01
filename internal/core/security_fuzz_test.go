package core

import "testing"

func FuzzRatingKeys(f *testing.F) {
	f.Add("1000000000000001")
	f.Add("9223372036854775807")
	f.Add("-1")
	f.Fuzz(func(t *testing.T, value string) {
		if key, e := ParseRatingKey(value); e == nil {
			if !key.Valid() {
				t.Fatal("invalid parsed key")
			}
			got, e := ParseRatingKey(key.String())
			if e != nil || got != key {
				t.Fatal("rating key roundtrip")
			}
			_ = key.Parent()
			_ = key.TMDB()
			_ = key.Season()
			_ = key.Episode()
		}
	})
}

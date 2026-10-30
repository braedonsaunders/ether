package ids

import (
	"strings"
	"testing"
)

func FuzzProtocolIDs(f *testing.F) {
	f.Add("000000000000000100038d7ea4c68001", int64(1))
	f.Add("-1", int64(-1))
	f.Fuzz(func(t *testing.T, value string, n int64) {
		for kind := Item; kind <= Programme; kind++ {
			wire, e := GUID(kind, n)
			if e != nil {
				t.Fatal(e)
			}
			got, e := ParseGUID(kind, wire)
			if e != nil || got != n {
				t.Fatal("GUID roundtrip")
			}
			if parsed, e := ParseGUID(kind, value); e == nil {
				canonical, _ := GUID(kind, parsed)
				if canonical != strings.ToLower(strings.ReplaceAll(value, "-", "")) {
					t.Fatal("GUID canonicalization")
				}
			}
		}
		if id, e := ParseDecimal(value); e == nil && Decimal(id) != value {
			t.Fatal("decimal canonicalization")
		}
		if key, e := ParseRatingKeyGUID(value); e == nil {
			if _, e = ParseRatingKeyGUID(RatingKeyGUID(key)); e != nil {
				t.Fatal(e)
			}
		}
	})
}

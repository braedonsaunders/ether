package ids

import (
	"math"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestGUIDNamespacesAndFormats(t *testing.T) {
	for _, n := range []int64{0, 1, -1, math.MaxInt64, math.MinInt64} {
		s, err := GUID(Item, n)
		if err != nil || len(s) != 32 {
			t.Fatalf("GUID: %q %v", s, err)
		}
		dashed := s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
		for _, v := range []string{s, dashed} {
			got, err := ParseGUID(Item, v)
			if err != nil || got != n {
				t.Fatalf("parse %q: %d %v", v, got, err)
			}
		}
		if _, err := ParseGUID(User, s); err == nil {
			t.Fatal("accepted another namespace")
		}
		key := core.RatingKey(n)
		got, err := ParseRatingKeyGUID(RatingKeyGUID(key))
		if err != nil || got != key {
			t.Fatal(got, err)
		}
	}
	for _, s := range []string{"", "00000000-000000000000000000000000", "xyz", "0000000000000001000000000000000-"} {
		if _, err := ParseGUID(Item, s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestDecimal(t *testing.T) {
	for _, n := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		got, err := ParseDecimal(Decimal(n))
		if err != nil || got != n {
			t.Fatal(got, err)
		}
	}
	for _, s := range []string{"+1", "01", "-0", "", "9223372036854775808"} {
		if _, err := ParseDecimal(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func FuzzGUID(f *testing.F) {
	f.Add(int64(1), uint8(1))
	f.Add(int64(-1), uint8(8))
	f.Fuzz(func(t *testing.T, n int64, raw uint8) {
		kind := Kind(raw%8 + 1)
		s, err := GUID(kind, n)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseGUID(kind, s)
		if err != nil || got != n {
			t.Fatal(got, err)
		}
	})
}
func FuzzParseGUID(f *testing.F) {
	f.Add("00000000000000010000000000000001")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseGUID(Item, s)
		if err == nil {
			encoded, _ := GUID(Item, n)
			again, err := ParseGUID(Item, encoded)
			if err != nil || n != again {
				t.Fatal(n, again, err)
			}
		}
	})
}

func TestStremio(t *testing.T) {
	for _, s := range []string{"tmdb:123", "tmdb:123:0:1", "tt0816692", "tt0903747:1:2"} {
		if _, err := ParseStremio(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"tmdb:0", "tmdb:01", "tt", "tt1:1", "tt1:-1:2", "tt1:1000:1", "ttx"} {
		if _, err := ParseStremio(s); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}
func FuzzStremio(f *testing.F) {
	f.Add("tt0903747:1:2")
	f.Add("tmdb:123")
	f.Fuzz(func(t *testing.T, s string) { _, _ = ParseStremio(s) })
}

func TestFormatStremio(t *testing.T) {
	for _, raw := range []string{"tmdb:123", "tmdb:123:0:1", "tt0816692", "tt0903747:1:2"} {
		id, err := ParseStremio(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := FormatStremio(id)
		if err != nil || got != raw {
			t.Fatal(got, err)
		}
	}
	if _, err := FormatStremio(StremioID{Source: "imdb", ID: "tt1", Season: 1}); err == nil {
		t.Fatal("ignored episode fields")
	}
}

func TestStringGUIDNamespaces(t *testing.T) {
	channel, err := StringGUID(Channel, "owner-channel")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := StringGUID(Channel, "owner-channel")
	programme, _ := StringGUID(Programme, "owner-channel")
	if channel != again || channel == programme || len(channel) != 32 {
		t.Fatal(channel, again, programme)
	}
	if _, err := ParseGUID(Programme, channel); err == nil {
		t.Fatal("accepted wrong namespace")
	}
	if _, err := StringGUID(Item, "owner-channel"); err == nil {
		t.Fatal("accepted wrong kind")
	}
}
func FuzzStringGUID(f *testing.F) {
	f.Add("channel-one")
	f.Fuzz(func(t *testing.T, s string) {
		if s == "" {
			return
		}
		first, err := StringGUID(Channel, s)
		if err != nil {
			t.Fatal(err)
		}
		second, _ := StringGUID(Channel, s)
		if first != second || len(first) != 32 {
			t.Fatal(first, second)
		}
	})
}

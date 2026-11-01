package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTitleUnknownLastAirDateIsOmitted(t *testing.T) {
	raw, err := json.Marshal(Title{Key: ShowKey(1), Type: TypeShow})
	if err != nil || strings.Contains(string(raw), "lastAirDate") {
		t.Fatalf("unknown date serialized: %s %v", raw, err)
	}
	when := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	raw, err = json.Marshal(Title{Key: ShowKey(1), Type: TypeShow, LastAirDate: when})
	if err != nil {
		t.Fatal(err)
	}
	var decoded Title
	if err = json.Unmarshal(raw, &decoded); err != nil || !decoded.LastAirDate.Equal(when) {
		t.Fatalf("date round-trip: %+v %v", decoded, err)
	}
	var old Title
	if err = json.Unmarshal([]byte(`{"type":"show"}`), &old); err != nil || !old.LastAirDate.IsZero() {
		t.Fatalf("older title payload: %+v %v", old, err)
	}
}

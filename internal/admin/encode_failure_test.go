package admin

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// An unencodable value used to go out as HTTP 200 with an empty body, which
// the console read as null (GitHub #180).
func TestWriteJSONUnencodableIsServerError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"x": math.NaN()})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"encode_failed"`) {
		t.Fatalf("body = %q, want an error body", rec.Body.String())
	}
}

func TestCollectionViewSkipsDamagedRuleValue(t *testing.T) {
	spec := core.CollectionSpec{
		ID: "c1",
		Rules: []core.CollectionRule{
			{Field: "genre", Op: "in", Value: json.RawMessage(`["Drama"]`)},
			{Field: "year", Op: "gte", Value: json.RawMessage(`{broken`)},
		},
		Exclude: []core.CollectionRule{{Field: "rating", Op: "in", Value: json.RawMessage(`nope`)}},
	}
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"collections": []collectionView{viewOf(spec)}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %q", rec.Code, rec.Body.String())
	}
	var got struct {
		Collections []core.CollectionSpec `json:"collections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	c := got.Collections[0]
	if len(c.Rules) != 1 || c.Rules[0].Field != "genre" || len(c.Exclude) != 0 {
		t.Fatalf("rules = %+v exclude = %+v", c.Rules, c.Exclude)
	}
	if len(c.Warnings) != 2 {
		t.Fatalf("warnings = %v, want one per skipped rule", c.Warnings)
	}
}

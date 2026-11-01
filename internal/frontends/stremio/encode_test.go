package stremio

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSONEncodesBeforeWritingStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, math.NaN())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "encode_failed") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

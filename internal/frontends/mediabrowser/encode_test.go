package mediabrowser

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONReplyEncodesBeforeWritingStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	jsonReply(rec, math.NaN())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "encode_failed") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

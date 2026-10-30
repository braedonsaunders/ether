package logredact

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestReviewBasicAndMaskPrefix(t *testing.T) {
	for _, input := range []string{"Basic dXNlcjpwYXNz", "token=redacted-secret"} {
		if got := String(input); got == input {
			t.Errorf("credential unchanged: %q", got)
		}
	}
}

func TestReviewStructuredCredentials(t *testing.T) {
	type payload struct {
		Password string `json:"password"`
		Count    int64  `json:"count"`
	}
	for _, jsonOutput := range []bool{false, true} {
		for _, grouped := range []bool{false, true} {
			var out bytes.Buffer
			var sink slog.Handler = slog.NewTextHandler(&out, nil)
			if jsonOutput {
				sink = slog.NewJSONHandler(&out, nil)
			}
			logger := slog.New(Handler(sink))
			if grouped {
				logger = logger.WithGroup("Authorization")
				logger.Info("request", slog.String("value", "group-secret"))
			} else {
				logger.With(slog.Any("request", map[string]any{"X-Plex-Token": "map-secret", "items": []any{payload{"struct-secret", 9007199254740993}}, "keep": "ordinary"})).Info("request", slog.Group("password", slog.String("value", "nested-secret")))
			}
			for _, secret := range []string{"map-secret", "struct-secret", "group-secret", "nested-secret"} {
				if strings.Contains(out.String(), secret) {
					t.Errorf("json=%v grouped=%v leaked %s: %s", jsonOutput, grouped, secret, out.String())
				}
			}
			if !grouped && (!strings.Contains(out.String(), "ordinary") || !strings.Contains(out.String(), "9007199254740993")) {
				t.Errorf("benign values lost: %s", out.String())
			}
		}
	}
}

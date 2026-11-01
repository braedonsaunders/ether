package conform

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	secret := `a"b`
	body, _ := json.Marshal(map[string]string{"Name": "lab owner", "Message": secret})
	r := Response{Body: body, Headers: http.Header{"Location": []string{"https://example.test/?token=secret"}}}
	out := RedactSecrets(r, "lab owner", secret, "secret")
	var decoded map[string]string
	if err := json.Unmarshal(out.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["Name"] != "[REDACTED]" || decoded["Message"] != "[REDACTED]" || strings.Contains(out.Headers.Get("Location"), "secret") {
		t.Fatal("sensitive value remained")
	}
}

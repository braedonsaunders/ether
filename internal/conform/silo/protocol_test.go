package silo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSiloBearerProfile(t *testing.T) {
	dir := t.TempDir()
	body := `{"protocol":"silo","client":"Silo","flows":{"connect":[{"name":"health","method":"GET","path":"/api/v2/health"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "apple.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing bearer authorization")
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err = p.Fetch(context.Background(), srv.Client(), srv.URL, "secret", p.Corpus("apple")[0], "apple", nil, true); err != nil {
		t.Fatal(err)
	}
}

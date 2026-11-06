package onair

import (
	"context"
	"reflect"
	"testing"
)

func TestOwnedTemplates(t *testing.T) {
	s := testService(t, Options{})
	a, e := s.Templates(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.Templates(context.Background())
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("templates must be deterministic", e)
	}
	for _, v := range a {
		if len(v.Style.Scenes) < 7 || v.Style.Template != v.ID {
			t.Fatalf("incomplete template %s", v.ID)
		}
		if e = s.validateLook(context.Background(), v.Style); e != nil {
			t.Fatal(v.ID, e)
		}
	}
}

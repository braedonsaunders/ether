package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"testing"
)

func TestJellyfinPlayAccessUsesHostEstimate(t *testing.T) {
	i := &instance{}
	yes, no := true, false
	for _, tc := range []struct {
		can  *bool
		want string
	}{{nil, ""}, {&yes, "Full"}, {&no, "None"}} {
		dto := map[string]any{}
		i.jellyfinItemDetailDTO(frontend.ItemDetail{CanPlay: tc.can}, dto)
		if got, _ := dto["PlayAccess"].(string); got != tc.want {
			t.Fatal(got, tc.want)
		}
		if _, ok := dto["Trickplay"]; ok {
			t.Fatal("non-video detail gained trickplay")
		}
	}
	dto := map[string]any{"MediaSources": []any{map[string]any{"Chapters": []any{map[string]any{"Name": "Opening"}}}}}
	i.jellyfinItemDetailDTO(frontend.ItemDetail{Title: frontend.Title{Type: "episode"}}, dto)
	chapters, _ := dto["Chapters"].([]any)
	if len(chapters) != 1 || dto["Trickplay"] == nil {
		t.Fatal(dto)
	}
	empty := map[string]any{}
	i.jellyfinItemDetailDTO(frontend.ItemDetail{Title: frontend.Title{Type: "movie"}}, empty)
	if _, ok := empty["Chapters"].([]any); !ok || empty["Trickplay"] == nil {
		t.Fatal(empty)
	}
}

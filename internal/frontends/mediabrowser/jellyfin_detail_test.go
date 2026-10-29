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
	}
}

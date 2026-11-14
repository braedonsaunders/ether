package frontend

import "testing"

func TestRankedArtworkIdentity(t *testing.T) {
	base := Image{Kind: ImagePoster, Path: "poster:source", Tag: "version", Subject: "", BlurHash: "old"}
	for rank := 1; rank <= 10; rank++ {
		for _, wide := range []bool{false, true} {
			img := RankedArtwork(base, rank, wide)
			got, n, w, err := UnrankArtwork(img)
			if err != nil || n != rank || w != wide || !SameImage(base, got) || img.BlurHash != "" {
				t.Fatalf("identity: %+v %d %t %v", got, n, w, err)
			}
		}
	}
	for _, path := range []string{"ranked-v1:11:wide:x", "ranked-v1:01:wide:x", "ranked-v1:1:other:x", "ranked-v1:1:wide:ranked-v1:1:wide:x"} {
		if _, _, _, err := UnrankArtwork(Image{Kind: ImagePoster, Path: path, Tag: path}); err == nil {
			t.Fatal("accepted malformed variant", path)
		}
	}
}

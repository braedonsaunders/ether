package frontend

import (
	"strconv"
	"strings"
)

const rankedArtworkPrefix = "ranked-v1:"

// RankedArtwork changes presentation only; the underlying descriptor retains
// its ordinary membership and visibility checks. Callers only issue it for a
// final authorized row position. No provider lookup or raster work occurs here.
func RankedArtwork(img Image, rank int, wide bool) Image {
	if rank < 1 || rank > 10 || img.Kind != ImagePoster || strings.HasPrefix(img.Path, rankedArtworkPrefix) {
		return img
	}
	shape := "portrait"
	img.Width, img.Height = 500, 750
	if wide {
		shape = "wide"
		img.Width, img.Height = 960, 540
	}
	prefix := rankedArtworkPrefix + strconv.Itoa(rank) + ":" + shape + ":"
	img.Path = prefix + img.Path
	img.Tag = prefix + img.Tag
	img.BlurHash, img.ThumbHash = "", ""
	return img
}

// UnrankArtwork rejects malformed/nested variants instead of treating them as
// source paths. The returned source is re-enumerated under current policy.
func UnrankArtwork(img Image) (Image, int, bool, error) {
	if !strings.HasPrefix(img.Path, rankedArtworkPrefix) {
		return img, 0, false, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(img.Path, rankedArtworkPrefix), ":", 3)
	if len(parts) != 3 || (img.Kind != ImagePoster && img.Kind != ImageThumb) {
		return Image{}, 0, false, ErrUnauthorized
	}
	rank, err := strconv.Atoi(parts[0])
	wide := parts[1] == "wide"
	prefix := rankedArtworkPrefix + parts[0] + ":" + parts[1] + ":"
	if err != nil || rank < 1 || rank > 10 || strconv.Itoa(rank) != parts[0] || (!wide && parts[1] != "portrait") || parts[2] == "" || strings.HasPrefix(parts[2], rankedArtworkPrefix) || !strings.HasPrefix(img.Tag, prefix) {
		return Image{}, 0, false, ErrUnauthorized
	}
	if img.Kind == ImageThumb {
		if !wide {
			return Image{}, 0, false, ErrUnauthorized
		}
		img.Kind = ImagePoster
	}
	img.Path = parts[2]
	img.Tag = strings.TrimPrefix(img.Tag, prefix)
	return img, rank, wide, nil
}

// RankedCardThumb exposes the same authorized poster as a wide Thumb for
// clients that choose landscape card surfaces independently of Primary.
func RankedCardThumb(img Image, rank int) Image {
	out := RankedArtwork(img, rank, true)
	if out.Path != img.Path {
		out.Kind = ImageThumb
	}
	return out
}

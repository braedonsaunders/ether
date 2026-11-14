package plex

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

func rankedMetadata(items []model.Metadata) { rankedMetadataOffset(items, 0) }
func rankedMetadataOffset(items []model.Metadata, offset int) {
	for n := range items {
		m := &items[n]
		if m.Type != "movie" && m.Type != "show" {
			continue
		}
		q := url.Values{"ranked": {strconv.Itoa(offset + n + 1)}, "rv": {m.Thumb}}
		portrait := "/library/metadata/" + m.RatingKey + "/thumb?" + q.Encode()
		q.Set("rankshape", "wide")
		wide := "/library/metadata/" + m.RatingKey + "/thumb?" + q.Encode()
		m.Thumb = portrait
		images := make([]model.Image, 0, len(m.Image)+2)
		for _, img := range m.Image {
			if img.Type != "coverPoster" && img.Type != "coverArt" {
				images = append(images, img)
			}
		}
		images = append(images, model.Image{Type: "coverPoster", URL: portrait}, model.Image{Type: "coverArt", URL: wide})
		m.Image = images
	}
}

func (s *Server) serveRankedArtwork(w http.ResponseWriter, r *http.Request, key frontend.RatingKey, width int) {
	rank, err := strconv.Atoi(r.URL.Query().Get("ranked"))
	shape := r.URL.Query().Get("rankshape")
	if err != nil || rank < 1 || rank > 10 || (shape != "" && shape != "wide") || (key.Kind() != frontend.KindMovie && key.Kind() != frontend.KindShow) {
		writeHTTPError(w, 404)
		return
	}
	art := s.getHost().Artwork
	if art == nil {
		writeHTTPError(w, 404)
		return
	}
	pr := FromContext(r.Context())
	req := frontend.ArtworkRequest{Viewer: pr.User, Item: key, Device: frontend.Device{Language: pr.Language, Product: pr.Product, Platform: pr.Platform}}
	images, complete, err := art.List(r.Context(), req)
	if err != nil || !complete {
		writeHTTPError(w, 404)
		return
	}
	for _, img := range images {
		if img.Kind == frontend.ImagePoster {
			data, ct, err := art.Read(r.Context(), req, frontend.RankedArtwork(img, rank, shape == "wide"), frontend.ArtworkTransform{Width: width})
			if err != nil {
				writeHTTPError(w, 404)
				return
			}
			setArtworkHeaders(w.Header(), ct)
			w.Header().Set("Cache-Control", "private, max-age=3600")
			w.Header().Add("Vary", "X-Plex-Token")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(data)
			}
			return
		}
	}
	writeHTTPError(w, 404)
}

func paintRankedCollection(mc *model.MediaContainer, offset int) {
	if offset >= 10 {
		mc.Metadata = nil
	} else {
		mc.Metadata = mc.Metadata[:min(len(mc.Metadata), 10-offset)]
	}
	rankedMetadataOffset(mc.Metadata, offset)
	total := 10
	if mc.TotalSize != nil {
		total = min(total, *mc.TotalSize)
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(min(offset, total), total)
	typ := "movie"
	if len(mc.Metadata) > 0 {
		typ = mc.Metadata[0].Type
	}
	mc.Meta = &model.Meta{DisplayImage: []model.DisplayImage{{Type: typ, ImageType: "coverArt"}}}
}

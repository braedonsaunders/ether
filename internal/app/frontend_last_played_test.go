package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestLastPlayedSourceRequiresCurrentItemPolicy(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	viewer, key := &core.Viewer{ID: 7}, core.MovieKey(1)
	epoch, err := a.state.BeginSourceSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.state.RecordLastSource(ctx, viewer.ID, key, epoch, core.PlaybackSourceObservation{Revision: "source", VideoCodec: "h264"}); err != nil {
		t.Fatal(err)
	}
	allowed := true
	experience := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if !allowed || req.Viewer != viewer || req.Key != key {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: key}}, nil
	}}
	reader := surfaceLastPlayedSource{experience: experience, state: a.state}
	if source, err := reader.LastPlayedSource(ctx, viewer, core.Device{ID: "tv"}, key); err != nil || source == nil || source.VideoCodec != "h264" {
		t.Fatalf("visible source: %+v %v", source, err)
	}
	allowed = false
	if source, err := reader.LastPlayedSource(ctx, viewer, core.Device{ID: "tv"}, key); err == nil || source != nil {
		t.Fatalf("restricted source exposed: %+v %v", source, err)
	}
}

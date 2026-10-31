package service

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestDetachedPersonImagesPreserveExistingReferences(t *testing.T) {
	ctx := context.Background()
	key := core.MovieKey(551)
	title := core.Title{Key: key, Type: core.TypeMovie, Released: time.Unix(1, 0), Cast: []core.Person{{TMDB: 1, Name: "Actor", ThumbPath: "/actor.jpg"}}, Directors: []core.Person{{TMDB: 2, Name: "Director", ThumbPath: "/director.jpg"}}, Writers: []core.Person{{TMDB: 3, Name: "Writer", ThumbPath: "/writer.jpg"}}, Crew: []core.Person{{TMDB: 4, Name: "Crew", ThumbPath: "/crew.jpg"}}}
	req := frontend.ArtworkRequest{Viewer: &core.User{ID: 7}, Item: key, Device: core.Device{Language: "fr-CA"}}
	want := []frontend.Image{}
	for _, spec := range []struct{ role, id, thumb string }{
		{"actor", "1", "/actor.jpg"},
		{"director", "2", "/director.jpg"},
		{"writer", "3", "/writer.jpg"},
	} {
		subject := "tmdb:" + spec.id
		ref := artworkReference(req, frontend.ImagePerson, spec.role+"\x00"+subject+"\x00"+spec.thumb)
		want = append(want, frontend.Image{Kind: frontend.ImagePerson, Path: ref, Tag: ref, Subject: subject})
	}
	detached := Source{Metadata: func(context.Context, frontend.ArtworkRequest, frontend.Image) frontend.Image {
		t.Fatal("detached person scheduled metadata work")
		return frontend.Image{}
	}}
	got, err := detached.PersonImages(ctx, req, title)
	if err != nil || len(got) != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("descriptor bytes/order changed\nwant=%+v\ngot=%+v err=%v", want, got, err)
	}
}

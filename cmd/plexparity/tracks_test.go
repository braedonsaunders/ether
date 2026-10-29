package main

import "testing"

func TestFixtureStreamsReadsIdentityAndSelection(t *testing.T) {
	streams, err := fixtureStreams([]byte(`<MediaContainer><Video><Media><Part><Stream id="42" streamType="2" index="2" languageCode="eng" selected="1"/><Stream id="43" streamType="3" index="3" languageCode="eng" selected="1"/></Part></Media></Video></MediaContainer>`))
	if err != nil || len(streams) != 2 || streams[0].ID != 42 || streams[0].Index != 2 || streams[1].Type != 3 || streams[1].Selected != "1" {
		t.Fatalf("streams=%+v error=%v", streams, err)
	}
	if _, err := fixtureStreams([]byte(`<MediaContainer><Stream`)); err == nil {
		t.Fatal("malformed response accepted")
	}
}

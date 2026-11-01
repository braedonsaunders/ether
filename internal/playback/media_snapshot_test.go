package playback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func snapshotFacts(t *testing.T) core.MediaInfo {
	t.Helper()
	stream := core.StreamInfo{Index: 3, Type: "video", Codec: "hevc", ExternalURL: "source-owned", Width: 1920, Height: 1080}
	fields := reflect.ValueOf(&stream).Elem()
	for i := 0; i < fields.NumField(); i++ {
		f := fields.Field(i)
		if f.Kind() != reflect.Pointer {
			continue
		}
		p := reflect.New(f.Type().Elem())
		switch p.Elem().Kind() {
		case reflect.Bool:
			p.Elem().SetBool(false) // explicit false must remain distinct from nil
		case reflect.Int:
			p.Elem().SetInt(4)
		case reflect.Float64:
			p.Elem().SetFloat(24)
		default:
			t.Fatalf("new mutable stream field needs snapshot coverage: %s", fields.Type().Field(i).Name)
		}
		f.Set(p)
	}
	return core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{stream}, Chapters: []core.Chapter{{ID: 1, Title: "original", Start: time.Second}}, Keyframes: []core.KeyframePoint{{Offset: 42, Time: time.Second}}}
}

func mutateSnapshot(info core.MediaInfo) {
	info.Streams[0].Codec, info.Streams[0].ExternalURL = "mutated", "mutated-source"
	fields := reflect.ValueOf(info.Streams[0])
	for i := 0; i < fields.NumField(); i++ {
		f := fields.Field(i)
		if f.Kind() != reflect.Pointer || f.IsNil() {
			continue
		}
		switch f.Elem().Kind() {
		case reflect.Bool:
			f.Elem().SetBool(true)
		case reflect.Int:
			f.Elem().SetInt(-1)
		case reflect.Float64:
			f.Elem().SetFloat(-1)
		}
	}
	info.Chapters[0].Title = "mutated"
	if len(info.Keyframes) > 0 {
		info.Keyframes[0].Offset = -1
	}
}

func TestGenerationMediaSnapshotDetachesIngressAndEgress(t *testing.T) {
	input, expected := snapshotFacts(t), snapshotFacts(t)
	before, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	g := &generation{}
	points := []IndexPoint{{Offset: 42, Time: time.Second}}
	g.publishInfo(input, core.PlaybackSourceObservation{}, TimeMap{Points: points})
	// Handoff is complete; the producer can now reuse its retained data.
	mutateSnapshot(input)
	points[0].Offset = -1
	one, two := g.infoSnapshot(), g.infoSnapshot()
	if !reflect.DeepEqual(one, expected) {
		t.Fatalf("retained producer changed generation: %+v", one)
	}
	after, err := json.Marshal(one)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("snapshot changed wire values: %s / %s: %v", before, after, err)
	}
	mutateSnapshot(one)
	if !reflect.DeepEqual(two, expected) || !reflect.DeepEqual(g.infoSnapshot(), expected) {
		t.Fatal("returned slice/pointer mutation changed another snapshot or generation")
	}
	if g.timeMapSnapshot().Points[0].Offset != 42 {
		t.Fatal("retained producer changed generation time map")
	}
}

func TestGenerationMediaSnapshotPreservesNilAndEmpty(t *testing.T) {
	for _, info := range []core.MediaInfo{
		{},
		{Streams: []core.StreamInfo{}, Chapters: []core.Chapter{}, Keyframes: []core.KeyframePoint{}},
		{Streams: []core.StreamInfo{{Type: "video"}}},
	} {
		g := &generation{}
		g.publishInfo(info, core.PlaybackSourceObservation{}, TimeMap{})
		if got := g.infoSnapshot(); !reflect.DeepEqual(got, info) {
			t.Fatalf("nil/empty or unknown fact changed: got%#v want%#v", got, info)
		}
	}
}

func TestGenerationMediaSnapshotsConcurrentOwnedMutation(t *testing.T) {
	input := snapshotFacts(t)
	expected := snapshotFacts(t)
	g := &generation{}
	g.publishInfo(input, core.PlaybackSourceObservation{}, TimeMap{})
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			mutateSnapshot(input)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			g.setIndex([]IndexPoint{{Offset: int64(i), Time: time.Second}})
		}
	}()
	for j := 0; j < 3; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				info := g.infoSnapshot()
				if !reflect.DeepEqual(info.Streams, expected.Streams) || !reflect.DeepEqual(info.Chapters, expected.Chapters) {
					errs <- fmt.Errorf("snapshot changed through another owner")
					return
				}
				mutateSnapshot(info)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

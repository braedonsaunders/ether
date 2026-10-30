package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// trickplayCapture waits for real FFmpeg-generated frames before comparing the
// BIF and JPEG endpoints; a missing frame is an invalid fixture, not coverage.
func trickplayCapture(ctx context.Context, client *http.Client, base, viewer, name, key string, profile http.Header) ([]capture, error) {
	h := profile.Clone()
	id := "parity-" + viewer + "-" + name + "-trickplay"
	h.Set("X-Plex-Client-Identifier", id)
	h.Set("X-Plex-Session-Identifier", id)
	h.Set("Accept", "application/xml")
	fetch := func(path string) (protocol.Response, error) {
		return strictFetch(ctx, client, base, "parity-"+viewer, protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{name: h}}, name, nil, false)
	}
	path := "/video/:/transcode/universal/decision?path=" + url.QueryEscape("/library/metadata/"+key) + "&protocol=http&session=" + id + "&directPlay=1&directStream=1"
	decision, err := fetch(path)
	if err != nil || decision.Status != http.StatusOK {
		return nil, fmt.Errorf("trickplay setup: status=%d error=%v", decision.Status, err)
	}
	part, err := fetch("/library/parts/" + key + "/file.mp4")
	if err != nil || part.Status != http.StatusOK || len(part.Body) == 0 {
		return nil, fmt.Errorf("trickplay source: status=%d error=%v", part.Status, err)
	}
	timeline, err := fetch("/:/timeline?ratingKey=" + key + "&state=playing&time=20000&duration=20000")
	if err != nil || timeline.Status != http.StatusOK {
		return nil, fmt.Errorf("trickplay timeline: status=%d error=%v", timeline.Status, err)
	}
	indexPath := "/library/parts/" + key + "/indexes/sd"
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	var bif protocol.Response
	for {
		bif, err = fetch(indexPath)
		if err != nil {
			return nil, err
		}
		if bif.Status == http.StatusOK {
			b, e := transcode.ParseBIF(bif.Body)
			if e != nil {
				return nil, fmt.Errorf("invalid BIF: %w", e)
			}
			// The 20-second fixture has only one keyframe at or before the
			// ten-second BIF grid point, so its complete sparse index has
			// exactly one generated image.
			if b.Len() == 1 {
				break
			}
		} else if bif.Status != http.StatusNotFound {
			return nil, fmt.Errorf("trickplay BIF status %d", bif.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("trickplay frames did not finish")
		case <-time.After(100 * time.Millisecond):
		}
	}
	frame, err := fetch(indexPath + "/0")
	if err != nil || frame.Status != http.StatusOK || len(frame.Body) == 0 {
		return nil, fmt.Errorf("trickplay frame: status=%d error=%v", frame.Status, err)
	}
	index, err := transcode.ParseBIF(bif.Body)
	if err != nil || !transcode.IsJPEG(frame.Body) || !bytes.Equal(index.Frame(0), frame.Body) {
		return nil, fmt.Errorf("trickplay frame does not match generated BIF: %v", err)
	}
	stopped, err := fetch("/:/timeline?ratingKey=" + key + "&state=stopped&time=20000&duration=20000")
	if err != nil || stopped.Status != http.StatusOK {
		return nil, fmt.Errorf("trickplay stop: status=%d error=%v", stopped.Status, err)
	}
	return []capture{{viewer, name, "trickplay:decision", false, decision}, {viewer, name, "trickplay:source", false, part}, {viewer, name, "trickplay:timeline", false, timeline}, {viewer, name, "trickplay:bif", false, bif}, {viewer, name, "trickplay:frame", false, frame}, {viewer, name, "trickplay:stop", false, stopped}}, nil
}

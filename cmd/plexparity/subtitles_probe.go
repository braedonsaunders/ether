package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// subtitleCapture discovers a real offer from the offline provider, picks it,
// and compares the cached sidecar and transcode-timeline responses.
func subtitleCapture(ctx context.Context, client *http.Client, base, viewer, name, key string, profile http.Header) ([]capture, error) {
	h := profile.Clone()
	id := "parity-" + viewer + "-" + name + "-subtitle"
	h.Set("X-Plex-Client-Identifier", id)
	h.Set("X-Plex-Session-Identifier", id)
	h.Set("Accept", "application/json")
	fetch := func(method, path string) (protocol.Response, error) {
		return strictFetch(ctx, client, base, "parity-"+viewer, protocol.Request{Method: method, Path: path, Headers: map[string]http.Header{name: h}}, name, nil, false)
	}
	search, err := fetch(http.MethodGet, "/library/metadata/"+key+"/subtitles?language=eng")
	if err != nil || search.Status != http.StatusOK {
		return nil, fmt.Errorf("subtitle search: status=%d error=%v", search.Status, err)
	}
	var offers struct {
		MediaContainer struct {
			Streams []struct {
				Key string `json:"key"`
			} `json:"Stream"`
		} `json:"MediaContainer"`
	}
	if e := json.Unmarshal(search.Body, &offers); e != nil || len(offers.MediaContainer.Streams) != 1 || !strings.HasPrefix(offers.MediaContainer.Streams[0].Key, "/library/metadata/"+key+"/subtitles/") {
		return nil, fmt.Errorf("subtitle offer discovery: %s (%v)", search.Body, e)
	}
	picked, err := fetch(http.MethodPut, "/library/metadata/"+key+"/subtitles?key="+url.QueryEscape(offers.MediaContainer.Streams[0].Key))
	if err != nil || picked.Status != http.StatusOK {
		return nil, fmt.Errorf("subtitle pick: status=%d error=%v", picked.Status, err)
	}
	metadata, err := fetch(http.MethodGet, "/library/metadata/"+key)
	if err != nil || metadata.Status != http.StatusOK {
		return nil, fmt.Errorf("subtitle metadata: status=%d error=%v", metadata.Status, err)
	}
	var m struct {
		MediaContainer struct {
			Metadata []struct {
				Media []struct {
					Part []struct {
						Streams []struct {
							ID       int64 `json:"id"`
							External bool  `json:"external"`
						} `json:"Stream"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if e := json.Unmarshal(metadata.Body, &m); e != nil {
		return nil, e
	}
	var stream int64
	for _, item := range m.MediaContainer.Metadata {
		for _, media := range item.Media {
			for _, part := range media.Part {
				for _, s := range part.Streams {
					if s.External && s.ID > 0 {
						stream = s.ID
					}
				}
			}
		}
	}
	if stream == 0 {
		return nil, fmt.Errorf("selected subtitle stream not discovered: %s", metadata.Body)
	}
	sid := strconv.FormatInt(stream, 10)
	sidecar, err := fetch(http.MethodGet, "/library/streams/"+sid)
	if err != nil || sidecar.Status != http.StatusOK || !bytes.Contains(sidecar.Body, []byte("Fixture subtitle")) {
		return nil, fmt.Errorf("subtitle stream: status=%d error=%v body=%s", sidecar.Status, err, sidecar.Body)
	}
	args := "path=" + url.QueryEscape("/library/metadata/"+key) + "&session=" + id + "&protocol=hls&directPlay=0&directStream=1&subtitleStreamID=" + sid
	start, err := fetch(http.MethodGet, "/video/:/transcode/universal/start.m3u8?"+args)
	if err != nil || start.Status != http.StatusOK || !bytes.HasPrefix(start.Body, []byte("#EXTM3U")) {
		return nil, fmt.Errorf("subtitle transcode start: status=%d error=%v body=%s", start.Status, err, start.Body)
	}
	subs, err := fetch(http.MethodGet, "/video/:/transcode/universal/subtitles?"+args)
	if err != nil || subs.Status != http.StatusOK || !bytes.Contains(subs.Body, []byte("Fixture subtitle")) {
		return nil, fmt.Errorf("subtitle transcode: status=%d error=%v body=%s", subs.Status, err, subs.Body)
	}
	stop, err := fetch(http.MethodGet, "/video/:/transcode/universal/stop?session="+id)
	if err != nil || stop.Status != http.StatusOK {
		return nil, fmt.Errorf("subtitle stop: status=%d error=%v", stop.Status, err)
	}
	return []capture{{viewer, name, "subtitle:search", true, search}, {viewer, name, "subtitle:pick", true, picked}, {viewer, name, "subtitle:metadata", true, metadata}, {viewer, name, "subtitle:stream", false, sidecar}, {viewer, name, "subtitle:start", false, start}, {viewer, name, "subtitle:transcode", false, subs}, {viewer, name, "subtitle:stop", true, stop}}, nil
}

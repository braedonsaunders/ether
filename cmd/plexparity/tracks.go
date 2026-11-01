package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/core"
)

type fixtureStream struct {
	ID       int64  `xml:"id,attr"`
	Type     int    `xml:"streamType,attr"`
	Index    int    `xml:"index,attr"`
	Language string `xml:"languageCode,attr"`
	Selected string `xml:"selected,attr"`
}

func fixtureStreams(body []byte) ([]fixtureStream, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var streams []fixtureStream
	for {
		token, err := d.Token()
		if err == io.EOF {
			return streams, nil
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Stream" {
			continue
		}
		var stream fixtureStream
		if err := d.DecodeElement(&stream, &start); err != nil {
			return nil, err
		}
		streams = append(streams, stream)
	}
}

// trackIdentity runs real estimate, selection, probe and selection again on a
// separate multitrack fixture. The actual file puts French before English,
// while the pre-play English estimate places its audio at index 1.
func trackIdentity(ctx context.Context, client *http.Client, base, viewer, name string, headers http.Header) ([]capture, error) {
	var captures []capture
	key := strconv.FormatInt(int64(core.MovieKey(10004)), 10)
	id := "parity-" + viewer + "-" + name + "-tracks"
	h := headers.Clone()
	h.Set("X-Plex-Client-Identifier", id)
	h.Set("X-Plex-Session-Identifier", id)
	h.Set("Accept", "application/xml")
	request := protocol.Request{Method: http.MethodGet, Path: "/library/metadata/" + key, Headers: map[string]http.Header{name: h}}
	// The detail read starts a warm probe and only waits 100ms. Main sometimes
	// still prints the estimate. Settle that probe first so both servers render
	// the probed file. This response is not part of the corpus.
	settle := request
	settle.Path = "/parity/warm-probe?ratingKey=" + key
	settled, err := strictFetch(ctx, client, base, "parity-"+viewer, settle, name, nil, false)
	if err != nil || settled.Status != http.StatusNoContent {
		return nil, fmt.Errorf("%s %s warm settle: status=%d error=%v", viewer, name, settled.Status, err)
	}
	fetch := func(label string) (protocol.Response, error) {
		r, err := strictFetch(ctx, client, base, "parity-"+viewer, request, name, nil, false)
		if err != nil || r.Status != http.StatusOK {
			return r, fmt.Errorf("%s %s track %s: status=%d error=%v", viewer, name, label, r.Status, err)
		}
		captures = append(captures, capture{viewer, name, "tracks:" + label, false, r})
		return r, nil
	}
	before, err := fetch("before")
	if err != nil {
		return nil, err
	}
	streams, err := fixtureStreams(before.Body)
	if err != nil {
		return nil, err
	}
	var estimated int64
	for _, stream := range streams {
		if stream.Type == 2 && strings.HasPrefix(stream.Language, "en") {
			estimated = stream.ID
			break
		}
	}
	if estimated == 0 {
		return nil, fmt.Errorf("%s %s missing estimated English audio", viewer, name)
	}
	request.Method = http.MethodPut
	request.Path = "/library/parts/" + key + "?audioStreamID=" + strconv.FormatInt(estimated, 10)
	if _, err = fetch("select-estimate"); err != nil {
		return nil, err
	}
	request.Method = http.MethodGet
	request.Path = "/video/:/transcode/universal/decision?path=" + url.QueryEscape("/library/metadata/"+key) + "&protocol=http&session=" + id + "&directPlay=1&directStream=1"
	probed, err := fetch("probe")
	if err != nil {
		return nil, err
	}
	streams, err = fixtureStreams(probed.Body)
	if err != nil {
		return nil, err
	}
	var audio, subtitle int64
	for _, stream := range streams {
		if stream.Type == 2 && strings.HasPrefix(stream.Language, "en") {
			audio = stream.ID
			if stream.Index != 2 || (stream.Selected != "1" && stream.Selected != "true") {
				return nil, fmt.Errorf("%s %s English audio was not remapped after probe: %+v", viewer, name, stream)
			}
		}
		if stream.Type == 3 && strings.HasPrefix(stream.Language, "en") {
			subtitle = stream.ID
		}
	}
	if audio == 0 || subtitle == 0 {
		return nil, fmt.Errorf("%s %s missing probed audio/subtitle", viewer, name)
	}
	request.Method = http.MethodPut
	request.Path = "/library/parts/" + key + "?audioStreamID=" + strconv.FormatInt(audio, 10) + "&subtitleStreamID=" + strconv.FormatInt(subtitle, 10)
	if _, err = fetch("select-probed"); err != nil {
		return nil, err
	}
	request.Method = http.MethodGet
	request.Path = "/video/:/transcode/universal/decision?path=" + url.QueryEscape("/library/metadata/"+key) + "&protocol=http&session=" + id + "&directPlay=1&directStream=1"
	after, err := fetch("selected")
	if err != nil {
		return nil, err
	}
	streams, err = fixtureStreams(after.Body)
	if err != nil {
		return nil, err
	}
	selectedAudio, selectedSubtitle := false, false
	for _, stream := range streams {
		selected := stream.Selected == "1" || stream.Selected == "true"
		if stream.ID == audio && selected {
			selectedAudio = true
		}
		if stream.ID == subtitle && selected {
			selectedSubtitle = true
		}
	}
	if !selectedAudio || !selectedSubtitle {
		return nil, fmt.Errorf("%s %s selected track IDs were not retained", viewer, name)
	}
	request.Path = "/:/timeline?ratingKey=" + key + "&state=stopped&time=0&duration=5400000"
	if _, err = fetch("stop"); err != nil {
		return nil, err
	}
	return captures, nil
}

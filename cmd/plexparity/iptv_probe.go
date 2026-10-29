package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// iptvCapture follows the real provider playlist and, separately, an actual
// transcoded relay. DASH representation identifiers come from its manifest.
func iptvCapture(ctx context.Context, client *http.Client, base, viewer, name string, profile http.Header) ([]capture, error) {
	h := profile.Clone()
	h.Set("Accept", "application/json")
	h.Set("X-Plex-Client-Identifier", "parity-"+viewer+"-"+name+"-iptv")
	fetch := func(method, path string) (protocol.Response, error) {
		return protocol.Fetch(ctx, client, base, "parity-"+viewer, protocol.Request{Method: method, Path: path, Headers: map[string]http.Header{name: h}}, name, nil, false)
	}
	var out []capture
	for _, ch := range []string{"fixture-iptv", "fixture-iptv-relay"} {
		tune, err := fetch(http.MethodPost, "/livetv/dvrs/1/channels/"+ch+"/tune")
		if err != nil {
			return nil, err
		}
		out = append(out, capture{viewer, name, "iptv:" + ch + ":tune", true, tune})
		if tune.Status != http.StatusOK {
			if (viewer == "teen" || viewer == "child") && (tune.Status == http.StatusForbidden || tune.Status == http.StatusNotFound) {
				continue
			}
			return nil, fmt.Errorf("IPTV %s tune: status=%d body=%s", ch, tune.Status, tune.Body)
		}
		match := liveSessionKey.FindSubmatch(tune.Body)
		if len(match) != 2 {
			return nil, fmt.Errorf("IPTV %s has no real session: %s", ch, tune.Body)
		}
		root := "/livetv/sessions/" + string(match[1])
		list, err := fetch(http.MethodGet, root+"/0/index.m3u8")
		if err != nil || list.Status != http.StatusOK || !bytes.HasPrefix(list.Body, []byte("#EXTM3U")) {
			return nil, fmt.Errorf("IPTV %s playlist: status=%d error=%v body=%s", ch, list.Status, err, list.Body)
		}
		if bytes.Contains(list.Body, []byte("/relay/")) {
			deadline := time.NewTimer(30 * time.Second)
			for !bytes.Contains(list.Body, []byte("#EXT-X-ENDLIST")) {
				select {
				case <-ctx.Done():
					deadline.Stop()
					return nil, ctx.Err()
				case <-deadline.C:
					return nil, fmt.Errorf("IPTV relay did not finish fixture playlist: %s", list.Body)
				case <-time.After(100 * time.Millisecond):
				}
				list, err = fetch(http.MethodGet, root+"/0/index.m3u8")
				if err != nil || list.Status != http.StatusOK {
					deadline.Stop()
					return nil, fmt.Errorf("IPTV relay playlist: status=%d error=%v", list.Status, err)
				}
			}
			deadline.Stop()
		}
		out = append(out, capture{viewer, name, "iptv:" + ch + ":playlist", false, list})
		var child string
		for _, line := range strings.Split(string(list.Body), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				child = line
				break
			}
		}
		if child == "" {
			return nil, fmt.Errorf("IPTV %s playlist missing media: %s", ch, list.Body)
		}
		u, err := url.Parse(child)
		if err != nil || !strings.HasPrefix(u.Path, root+"/0/") || (!strings.Contains(u.Path, "/resource/") && !strings.Contains(u.Path, "/relay/")) {
			return nil, fmt.Errorf("IPTV %s unexpected media path: %s", ch, child)
		}
		body, err := fetch(http.MethodGet, u.RequestURI())
		if err != nil || body.Status != http.StatusOK || len(body.Body) == 0 {
			return nil, fmt.Errorf("IPTV %s segment: status=%d error=%v", ch, body.Status, err)
		}
		mediaKind := "resource"
		if strings.Contains(u.Path, "/relay/") {
			mediaKind = "relay"
		}
		if ch == "fixture-iptv-relay" && mediaKind != "relay" {
			return nil, fmt.Errorf("IPTV transcoding fixture did not expose relay media: %s", child)
		}
		if ch == "fixture-iptv" && viewer == "owner" && name == "web" && mediaKind != "resource" {
			return nil, fmt.Errorf("IPTV passthrough fixture did not expose resource media: %s", child)
		}
		out = append(out, capture{viewer, name, "iptv:" + ch + ":" + mediaKind + ":media", false, body})
		if ch == "fixture-iptv" {
			var mpd protocol.Response
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			for {
				mpd, err = fetch(http.MethodGet, "/video/:/transcode/universal/start.mpd?path="+url.QueryEscape(root)+"&protocol=dash")
				if err != nil || mpd.Status != http.StatusOK {
					return nil, fmt.Errorf("IPTV DASH: status=%d error=%v body=%s", mpd.Status, err, mpd.Body)
				}
				// The fixed 20-second source yields five four-second segments.
				// Wait until both representations have published all five rather
				// than recording a scheduler-dependent partial manifest.
				if bytes.Count(mpd.Body, []byte(`r="4"`)) == 2 {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-deadline.C:
					return nil, fmt.Errorf("IPTV DASH did not finish its fixture segments: %s", mpd.Body)
				case <-time.After(100 * time.Millisecond):
				}
			}
			deadline.Stop()
			out = append(out, capture{viewer, name, "iptv:dash:manifest", false, mpd})
			dec := xml.NewDecoder(bytes.NewReader(mpd.Body))
			init, representation := "", ""
			for {
				tok, e := dec.Token()
				if e == io.EOF {
					break
				}
				if e != nil {
					return nil, e
				}
				elem, ok := tok.(xml.StartElement)
				if !ok {
					continue
				}
				for _, a := range elem.Attr {
					if elem.Name.Local == "SegmentTemplate" && a.Name.Local == "initialization" && init == "" {
						init = a.Value
					}
					if elem.Name.Local == "Representation" && a.Name.Local == "id" && representation == "" {
						representation = a.Value
					}
				}
			}
			if init == "" || representation == "" {
				return nil, fmt.Errorf("IPTV DASH manifest missing initialization or representation: %s", mpd.Body)
			}
			target := strings.ReplaceAll(init, "$RepresentationID$", representation)
			uri, e := url.Parse(target)
			if e != nil || !strings.HasPrefix(uri.Path, root+"/0/dash/") {
				return nil, fmt.Errorf("IPTV DASH invalid initialization: %s", target)
			}
			part, e := fetch(http.MethodGet, uri.RequestURI())
			if e != nil || part.Status != http.StatusOK || len(part.Body) == 0 {
				return nil, fmt.Errorf("IPTV DASH initialization: status=%d error=%v", part.Status, e)
			}
			out = append(out, capture{viewer, name, "iptv:dash:part", false, part})
		}
		stop, err := fetch(http.MethodDelete, root)
		if err != nil || stop.Status != http.StatusOK {
			return nil, fmt.Errorf("IPTV stop: status=%d error=%v", stop.Status, err)
		}
		out = append(out, capture{viewer, name, "iptv:" + ch + ":stop", true, stop})
	}
	return out, nil
}

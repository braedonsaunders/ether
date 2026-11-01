package conform

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// ComparePlaylist compares HLS structure without comparing signed URLs,
// segment counts, timestamps or media-specific values across servers.
func ComparePlaylist(a, b Response) Report {
	var r Report
	add := func(path, kind string, ref, target any) {
		r.Differences = append(r.Differences, Difference{Path: path, Kind: kind, Reference: ref, Target: target, Blocking: true})
	}
	if a.Status != b.Status {
		add("$status", KindStatus, a.Status, b.Status)
	}
	if a.Headers.Get("Content-Type") != b.Headers.Get("Content-Type") {
		add("$headers.Content-Type", KindHeader, a.Headers.Get("Content-Type"), b.Headers.Get("Content-Type"))
	}
	parse := func(body []byte) (map[string]bool, error) {
		lines := strings.Split(string(bytes.TrimSpace(body)), "\n")
		if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
			return nil, fmt.Errorf("missing EXTM3U header")
		}
		tags := map[string]bool{}
		for _, line := range lines[1:] {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "#EXT") {
				tag := strings.SplitN(line, ":", 2)[0]
				tags[tag] = true
			} else if !strings.HasPrefix(line, "#") {
				tags["URI"] = true
			}
		}
		return tags, nil
	}
	x, xe := parse(a.Body)
	y, ye := parse(b.Body)
	if xe != nil || ye != nil {
		add("$playlist", KindParse, errString(xe), errString(ye))
		return r
	}
	keys := map[string]bool{}
	for k := range x {
		keys[k] = true
	}
	for k := range y {
		keys[k] = true
	}
	list := make([]string, 0, len(keys))
	for k := range keys {
		list = append(list, k)
	}
	sort.Strings(list)
	for _, k := range list {
		if x[k] && !y[k] {
			add("$playlist/"+k, KindMissing, true, nil)
		} else if y[k] && !x[k] {
			add("$playlist/"+k, KindExtra, nil, true)
		}
	}
	return r
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// downloadCapture exercises an actual queue and waits for its remuxed file.
// Denied viewers retain their denial response; IDs are never invented.
func downloadCapture(ctx context.Context, client *http.Client, base, viewer, name, key string, profile http.Header) ([]capture, error) {
	h := profile.Clone()
	h.Set("Accept", "application/json")
	h.Set("X-Plex-Client-Identifier", "parity-"+viewer+"-"+name+"-download")
	fetch := func(method, path string) (protocol.Response, error) {
		return strictFetch(ctx, client, base, "parity-"+viewer, protocol.Request{Method: method, Path: path, Headers: map[string]http.Header{name: h}}, name, nil, false)
	}
	created, err := fetch(http.MethodPost, "/downloadQueue")
	if err != nil {
		return nil, err
	}
	if created.Status != http.StatusOK {
		if viewer != "owner" && created.Status == http.StatusForbidden {
			return []capture{{viewer, name, "downloads:create", true, created}}, nil
		}
		return nil, fmt.Errorf("download queue create: status=%d body=%s", created.Status, created.Body)
	}
	var q struct {
		MediaContainer struct {
			Queues []struct {
				ID int64 `json:"id"`
			} `json:"DownloadQueue"`
		} `json:"MediaContainer"`
	}
	if e := json.Unmarshal(created.Body, &q); e != nil || len(q.MediaContainer.Queues) != 1 || q.MediaContainer.Queues[0].ID <= 0 {
		return nil, fmt.Errorf("download queue discovery: %s (%v)", created.Body, e)
	}
	root := "/downloadQueue/" + strconv.FormatInt(q.MediaContainer.Queues[0].ID, 10)
	added, err := fetch(http.MethodPost, root+"/add?keys="+url.QueryEscape("/library/metadata/"+key))
	if err != nil || added.Status != http.StatusOK {
		return nil, fmt.Errorf("download add: status=%d error=%v body=%s", added.Status, err, added.Body)
	}
	var a struct {
		MediaContainer struct {
			Items []struct {
				ID int64 `json:"id"`
			} `json:"AddedQueueItems"`
		} `json:"MediaContainer"`
	}
	if e := json.Unmarshal(added.Body, &a); e != nil || len(a.MediaContainer.Items) != 1 || a.MediaContainer.Items[0].ID <= 0 {
		return nil, fmt.Errorf("download item discovery: %s (%v)", added.Body, e)
	}
	item := strconv.FormatInt(a.MediaContainer.Items[0].ID, 10)
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		r, e := fetch(http.MethodGet, root+"/items/"+item)
		if e != nil || r.Status != http.StatusOK {
			return nil, fmt.Errorf("download item poll: status=%d error=%v", r.Status, e)
		}
		var v struct {
			MediaContainer struct {
				Items []struct {
					Status string `json:"status"`
				} `json:"DownloadQueueItem"`
			} `json:"MediaContainer"`
		}
		if e = json.Unmarshal(r.Body, &v); e != nil || len(v.MediaContainer.Items) != 1 {
			return nil, fmt.Errorf("download status discovery: %s (%v)", r.Body, e)
		}
		if v.MediaContainer.Items[0].Status == "available" {
			break
		}
		if v.MediaContainer.Items[0].Status == "error" {
			return nil, fmt.Errorf("download failed: %s", r.Body)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("download did not finish: %s", r.Body)
		case <-time.After(100 * time.Millisecond):
		}
	}
	out := []capture{{viewer, name, "downloads:create", true, created}, {viewer, name, "downloads:add", true, added}}
	for _, probe := range []struct{ label, path string }{
		{"queue", root}, {"items", root + "/items"}, {"item", root + "/items/" + item}, {"decision", root + "/item/" + item + "/decision"}, {"media", root + "/item/" + item + "/media"},
	} {
		r, e := fetch(http.MethodGet, probe.path)
		if e != nil || r.Status != http.StatusOK || len(r.Body) == 0 {
			return nil, fmt.Errorf("download %s: status=%d error=%v", probe.label, r.Status, e)
		}
		out = append(out, capture{viewer, name, "downloads:" + probe.label, probe.label != "media", r})
	}
	return out, nil
}

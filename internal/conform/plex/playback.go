package plex

import "net/http"

// PlaybackCorpus is deliberately separate from the read-only browsing corpus:
// recording it changes queues and watch state and starts an encoder. Callers
// must use a disposable reference library and supply queueID/session variables.
// Tests run these probes offline with deterministic fake playback.
func PlaybackCorpus() []Request {
	probes := []Request{
		{Name: "queue-create", Method: http.MethodPost, Path: "/playQueues?type=video&uri=server%3A%2F%2F{{machineID}}%2Fcom.plexapp.plugins.library%2Flibrary%2Fmetadata%2F{{movieKey}}"},
		{Name: "queue-read", Method: http.MethodGet, Path: "/playQueues/{{queueID}}"},
		{Name: "timeline", Method: http.MethodGet, Path: "/:/timeline?ratingKey={{movieKey}}&key=%2Flibrary%2Fmetadata%2F{{movieKey}}&state=playing&time=12000&duration=60000"},
		{Name: "scrobble", Method: http.MethodGet, Path: "/:/scrobble?identifier=com.plexapp.plugins.library&key={{movieKey}}"},
		{Name: "unscrobble", Method: http.MethodGet, Path: "/:/unscrobble?identifier=com.plexapp.plugins.library&key={{movieKey}}"},
		{Name: "transcode-start", Method: http.MethodGet, Path: "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&session={{session}}&protocol=hls&directPlay=0&directStream=1"},
		{Name: "transcode-playlist", Method: http.MethodGet, Path: "/video/:/transcode/universal/session/{{session}}/base/index.m3u8"},
		{Name: "transcode-segment", Method: http.MethodGet, Path: "/video/:/transcode/universal/session/{{session}}/base/00000.ts"},
		{Name: "transcode-stop", Method: http.MethodGet, Path: "/video/:/transcode/universal/stop?session={{session}}"},
	}
	for i := range probes {
		probes[i].Headers = map[string]http.Header{}
		for profile, client := range Clients() {
			h := client.Headers.Clone()
			h.Set("Accept", "application/xml")
			probes[i].Headers[profile] = h
			h = h.Clone()
			h.Set("Accept", "application/json")
			probes[i].Headers[profile+"-json"] = h
		}
	}
	return probes
}

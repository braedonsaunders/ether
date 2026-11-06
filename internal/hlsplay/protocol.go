package hlsplay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (p *player) open(ctx context.Context) (string, []byte, error) {
	if p.isMediaBrowser() {
		return p.openMediaBrowser(ctx)
	}
	c := p.c
	p.playSession = c.Session
	q := url.Values{"path": {"/library/metadata/" + c.RatingKey}, "session": {c.Session}, "protocol": {"hls"}, "directPlay": {"0"}, "directStream": {"0"}, "videoCodec": {"h264"}, "audioCodec": {"aac"}, "mediaIndex": {"0"}, "partIndex": {"0"}, "offset": {"0"}}
	if c.DirectPlay {
		q.Set("directPlay", "1")
	}
	if c.DirectStream {
		q.Set("directStream", "1")
	}
	if c.MaxVideoBitrate > 0 {
		q.Set("maxVideoBitrate", strconv.Itoa(c.MaxVideoBitrate))
	}
	if c.VideoResolution != "" {
		q.Set("videoResolution", c.VideoResolution)
	}
	control := "/video/:/transcode/universal/"
	if !c.SkipDecision {
		if _, e := p.fetch(ctx, control+"decision?"+q.Encode(), "decision", -1, false); e != nil {
			return "", nil, e
		}
	}
	path := control + "start.m3u8?" + q.Encode()
	data, e := p.fetch(ctx, path, "master", -1, false)
	u, _ := resolve(c.BaseURL, path)
	return u, data, e
}
func (p *player) openMediaBrowser(ctx context.Context) (string, []byte, error) {
	prefix := strings.TrimSuffix(p.c.Prefix, "/")
	if p.c.CredentialsFile != "" {
		data, e := os.ReadFile(p.c.CredentialsFile)
		if e != nil {
			return "", nil, fmt.Errorf("cannot read lab credentials")
		}
		var credentials struct{ Username, Password string }
		if json.Unmarshal(data, &credentials) != nil || credentials.Username == "" || credentials.Password == "" {
			return "", nil, fmt.Errorf("invalid lab credentials")
		}
		body, _ := json.Marshal(map[string]string{"Username": credentials.Username, "Pw": credentials.Password})
		data, e = p.request(ctx, prefix+"/Users/AuthenticateByName", "authenticate", -1, false, "POST", body)
		if e != nil {
			return "", nil, e
		}
		var auth struct{ AccessToken string }
		if json.Unmarshal(data, &auth) != nil || auth.AccessToken == "" {
			return "", nil, fmt.Errorf("authentication returned no grant")
		}
		p.c.Token = auth.AccessToken
	}
	if _, e := p.fetch(ctx, prefix+"/Users/Me", "authenticate-viewer", -1, false); e != nil {
		return "", nil, e
	}
	key, e := strconv.ParseUint(p.c.RatingKey, 10, 64)
	if e != nil || key == 0 {
		return "", nil, fmt.Errorf("invalid rating key")
	}
	p.itemID = ids.RatingKeyGUID(core.RatingKey(key))
	// A normal Jellyfin HLS profile: closed MPEG-TS H264/AAC output, source
	// direct play disabled so PlaybackInfo admits an actual transcoding session.
	profile := map[string]any{"Name": p.mediaBrowserProduct(), "MaxStreamingBitrate": 1000000, "TranscodingProfiles": []any{map[string]any{"Type": "Video", "Context": "Streaming", "Protocol": "hls", "Container": "ts", "VideoCodec": "h264", "AudioCodec": "aac", "MaxAudioChannels": "2", "MinSegments": 1, "BreakOnNonKeyFrames": false}}}
	body, _ := json.Marshal(map[string]any{"IsPlayback": true, "EnableDirectPlay": false, "EnableDirectStream": false, "EnableTranscoding": true, "StartTimeTicks": 0, "DeviceProfile": profile, "MaxStreamingBitrate": 1000000})
	data, e := p.request(ctx, prefix+"/Items/"+p.itemID+"/PlaybackInfo", "playback-info", -1, false, "POST", body)
	if e != nil {
		return "", nil, e
	}
	var info struct {
		PlaySessionId, ErrorCode string
		MediaSources             []struct{ Id, TranscodingUrl, TranscodingSubProtocol string }
	}
	if json.Unmarshal(data, &info) != nil || info.ErrorCode != "" || info.PlaySessionId == "" || len(info.MediaSources) == 0 {
		return "", nil, fmt.Errorf("PlaybackInfo returned no playable session")
	}
	p.playSession = info.PlaySessionId
	p.mediaSource = info.MediaSources[0].Id
	p.event(map[string]any{"kind": "session", "session": p.playSession, "protocol": p.c.Protocol})
	source := info.MediaSources[0]
	if source.TranscodingUrl == "" || source.TranscodingSubProtocol != "hls" {
		return "", nil, fmt.Errorf("PlaybackInfo did not author HLS")
	}
	authored, e := resolve(p.c.BaseURL, source.TranscodingUrl)
	if e != nil {
		return "", nil, e
	}
	data, e = p.fetch(ctx, authored, "master", -1, false)
	return authored, data, e
}
func (p *player) progress(ctx context.Context, state string, played float64, started bool) error {
	if p.isMediaBrowser() {
		if p.playSession == "" {
			return nil
		}
		tail := "/Sessions/Playing/Progress"
		if started {
			tail = "/Sessions/Playing"
		}
		if state == "stopped" {
			tail = "/Sessions/Playing/Stopped"
		}
		body, _ := json.Marshal(map[string]any{"PlaySessionId": p.playSession, "ItemId": p.itemID, "MediaSourceId": p.mediaSource, "PositionTicks": int64(played * 10000000), "IsPaused": false, "PlayMethod": "Transcode"})
		_, e := p.request(ctx, strings.TrimSuffix(p.c.Prefix, "/")+tail, "timeline", -1, false, "POST", body)
		return e
	}
	q := url.Values{"ratingKey": {p.c.RatingKey}, "key": {"/library/metadata/" + p.c.RatingKey}, "state": {state}, "time": {strconv.FormatInt(int64(played*1000), 10)}, "duration": {"0"}, "session": {p.c.Session}}
	_, e := p.fetch(ctx, "/:/timeline?"+q.Encode(), "timeline", -1, false)
	return e
}
func (p *player) finish(ctx context.Context, played float64) {
	if p.isMediaBrowser() {
		_ = p.progress(ctx, "stopped", played, false)
		return
	}
	_, _ = p.fetch(ctx, "/video/:/transcode/universal/stop?session="+url.QueryEscape(p.c.Session), "stop", -1, false)
}

func (p *player) isMediaBrowser() bool { return p.c.Protocol == "jellyfin" || p.c.Protocol == "emby" }
func (p *player) mediaBrowserProduct() string {
	if p.c.Protocol == "emby" {
		return "Emby Web"
	}
	return "Jellyfin Web"
}

//go:build conformlab

package mediabrowser

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run with a locally forwarded, loopback-only scorecard listener:
// LAB_ENV=/path/to/lab.env LAB_JELLYFIN_URL=http://reference:8096 SCORECARD_URL=http://127.0.0.1:18196 SCORECARD_ORIGIN=http://127.0.0.1:18096 SCORECARD_MANIFEST=/path/to/scorecard-files.json go test -tags conformlab ./internal/conform/mediabrowser -run TestLabPlaybackDecisions -count=1 -v
// The manifest is private fixture input. No credentials or media paths are logged.
func TestLabPlaybackDecisions(t *testing.T) {
	envPath, refBase, targetBase, manifest := os.Getenv("LAB_ENV"), os.Getenv("LAB_JELLYFIN_URL"), os.Getenv("SCORECARD_URL"), os.Getenv("SCORECARD_MANIFEST")
	if envPath == "" || refBase == "" || targetBase == "" || manifest == "" {
		t.Skip("lab and scorecard environment not configured")
	}
	u, err := url.Parse(targetBase)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Scheme != "http" {
		t.Fatal("scorecard endpoint must use loopback HTTP")
	}
	settings, err := decisionLabSettings(envPath)
	if err != nil {
		t.Fatal("cannot read lab credentials")
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal("cannot read scorecard manifest")
	}
	var files []struct {
		TMDB    int64  `json:"tmdb"`
		Season  int    `json:"season"`
		Episode int    `json:"episode"`
		Path    string `json:"path"`
		SHA     string `json:"sha256"`
	}
	if json.Unmarshal(raw, &files) != nil {
		t.Fatal("invalid scorecard manifest")
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	device := "decision-" + hex.EncodeToString(nonce[:])
	t.Logf("test device=%s", device)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && (req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme) {
			return fmt.Errorf("redirect escaped server")
		}
		return nil
	}}
	reference := decisionLabServer{base: strings.TrimRight(refBase, "/"), device: device, client: client}
	target := decisionLabServer{base: strings.TrimRight(targetBase, "/"), origin: os.Getenv("SCORECARD_ORIGIN"), device: device, client: client}
	if err = reference.login(settings["JELLYFIN_ADULT_USER"], settings["JELLYFIN_ADULT_PASSWORD"]); err != nil {
		t.Fatal("reference sign-in failed", err)
	}
	defer func() {
		if err := reference.logout(); err != nil {
			t.Errorf("reference sign-out failed: %v", err)
		}
	}()
	if err = target.login("fixture-viewer", "fixture-password"); err != nil {
		t.Fatal("fixture sign-in failed", err)
	}
	defer func() {
		if err := target.logout(); err != nil {
			t.Errorf("target sign-out failed: %v", err)
		}
	}()
	refItems, err := reference.items()
	if err != nil {
		t.Fatal("reference item listing failed", err)
	}
	targetItems, err := target.items()
	if err != nil {
		t.Fatal("fixture item listing failed", err)
	}
	targetID := ""
	for _, it := range targetItems {
		if it.ProviderIDs["Tmdb"] == "10378" {
			targetID = it.ID
			break
		}
	}
	if targetID == "" {
		t.Fatal("fixture BBB is missing")
	}
	type matched struct{ referenceItem, referenceSource, variant, hash string }
	var matches []matched
	for _, item := range refItems {
		if item.ProviderIDs["Tmdb"] != "10378" {
			continue
		}
		for _, source := range item.MediaSources {
			for _, file := range files {
				if file.TMDB == 10378 && file.Season == 0 && file.Episode == 0 && source.Path != "" && source.Path == file.Path && len(file.SHA) == 64 {
					matches = append(matches, matched{item.ID, source.ID, fmt.Sprintf("scorecard-%d-%d-%d-%s", file.TMDB, file.Season, file.Episode, strings.ToLower(file.SHA)), strings.ToLower(file.SHA)})
				}
			}
		}
	}
	if len(matches) == 0 {
		t.Fatal("no reference source matches the scorecard manifest")
	}
	for _, profileName := range []string{"jellycon-default", "streamyfin-android-exoplayer"} {
		p, err := os.ReadFile(filepath.Join("..", "profiles", "jellyfin", "deviceprofiles", profileName+".json"))
		if err != nil {
			t.Fatal("missing client profile")
		}
		var profile struct {
			Client, Source, Configuration string
			Commit                        string          `json:"source_commit"`
			DeviceProfile                 json.RawMessage `json:"device_profile"`
		}
		if json.Unmarshal(p, &profile) != nil || len(profile.DeviceProfile) == 0 {
			t.Fatal("invalid client profile")
		}
		for _, m := range matches {
			t.Run(profileName+"/"+m.hash[:12], func(t *testing.T) {
				t.Logf("request reference POST /Items/%s/PlaybackInfo?IsPlayback=true MediaSourceId=%s profile=%s authorization=[redacted]", m.referenceItem, m.referenceSource, profileName)
				t.Logf("request target POST /Items/%s/PlaybackInfo?IsPlayback=true MediaSourceId=%s profile=%s authorization=[redacted]", targetID, m.variant, profileName)
				refResult, err := reference.playback(m.referenceItem, m.referenceSource, profile.DeviceProfile)
				if err != nil {
					t.Fatal("reference PlaybackInfo failed", err)
				}
				t.Log("reference PlaybackInfo HTTP 200")
				if refResult.Session != "" {
					defer func() {
						if err := reference.stop(refResult.Session, m.referenceItem); err != nil {
							t.Errorf("reference playback stop failed: %v", err)
						}
					}()
				}
				targetResult, err := target.playback(targetID, m.variant, profile.DeviceProfile)
				if err != nil {
					t.Fatal("target PlaybackInfo failed", err)
				}
				if targetResult.Session == "" {
					t.Fatal("target omitted playback session")
				}
				defer func() {
					if err := target.stop(targetResult.Session, targetID); err != nil {
						t.Errorf("target playback stop failed: %v", err)
					}
				}()
				refSource, refOK := exactDecisionSource(refResult.Sources, m.referenceSource)
				targetSource, targetOK := exactDecisionSource(targetResult.Sources, m.variant)
				if !refOK || !targetOK {
					t.Fatal("PlaybackInfo did not select the requested source")
				}
				if strings.Contains(targetSource.Path, target.token) || strings.Contains(targetSource.TranscodingURL, target.token) {
					t.Fatal("account credential appeared in a target stream URL")
				}
				refDelivery, err := RequestedDelivery(profileName, refSource)
				if err != nil {
					t.Fatal("reference client delivery unavailable")
				}
				targetDelivery, err := RequestedDelivery(profileName, targetSource)
				if err != nil {
					t.Fatal("target client delivery unavailable")
				}
				observed, err := target.broker(targetResult.Session)
				if err != nil {
					t.Fatal("missing broker diagnostic", err)
				}
				if observed.ID != targetResult.Session || observed.ClientID != "8:jellyfin"+device || observed.PlaybackID == "" {
					t.Fatal("broker session binding mismatch")
				}
				verified, err := target.verify(observed.PlaybackID)
				if err != nil {
					t.Fatal("source verification failed", err)
				}
				if verified.ReleaseID != m.variant || verified.SourceID != m.variant || verified.Expected != m.hash || verified.Actual != m.hash || !verified.Match {
					t.Fatal("selected source identity or hash differs")
				}
				t.Logf("verified client=%s source_commit=%s reference_item=%s variant=%s source_sha256=%s", profile.Client, profile.Commit, m.referenceItem, m.variant, m.hash)
				if refDelivery == "direct" {
					if err := reference.readDirect(profileName, m.referenceItem, refSource, refResult.Session); err != nil {
						t.Fatal("reference client direct request failed", err)
					}
					t.Log("reference client direct read succeeded")
				}
				t.Logf("requested delivery: reference=%s target=%s", refDelivery, targetDelivery)
				if targetDelivery == "direct" {
					if profileName == "jellycon-default" {
						t.Logf("request target GET /Videos/%s/stream?static=true&PlaySessionId=[redacted]&MediaSourceId=%s authorization=[redacted]", m.variant, m.variant)
					}
					if err := target.readDirect(profileName, targetID, targetSource, targetResult.Session); err != nil {
						t.Fatal("target client direct request failed", err)
					}
				}

				if targetDelivery == "transcode" && targetSource.TranscodingURL != "" {
					if err = target.readHLS(targetSource.TranscodingURL); err != nil {
						t.Fatal("could not observe HLS stream", err)
					}
					observed, err = target.broker(targetResult.Session)
					if err != nil {
						t.Fatal("missing active broker diagnostic", err)
					}
				}
				t.Logf("observed format=%s transcoder_active=%t transcoder=%+v", observed.Format, observed.TranscoderActive, observed.Transcoder)
				if refDelivery == "direct" {
					if targetDelivery != "direct" {
						t.Error("client requests transcoding from Ether where it requests direct delivery from Jellyfin")
					}
					if observed.Plan.Kind == "" || ((observed.Format == "hls" || observed.Plan.Kind == "transcode" || observed.TranscoderActive) && (!observed.TranscoderActive || observed.Transcoder == nil)) {
						t.Fatal("actual transcoder decision is unknown")
					}
					if observed.Transcoder != nil && (!observed.Transcoder.VideoCopy || !observed.Transcoder.AudioCopy) {
						t.Error("Ether encodes media where the reference client requests direct delivery")
					}
				}
				t.Logf("client=%s commit=%s configuration=%q reference_item=%s source_sha256=%s reference_delivery=%s target_delivery=%s", profile.Client, profile.Commit, profile.Configuration, m.referenceItem, m.hash, refDelivery, targetDelivery)
			})
		}
	}
}

type decisionItem struct {
	ID           string                      `json:"Id"`
	ProviderIDs  map[string]string           `json:"ProviderIds"`
	MediaSources []struct{ ID, Path string } `json:"MediaSources"`
}
type decisionReply struct {
	Session string          `json:"PlaySessionId"`
	Sources []PlaybackOffer `json:"MediaSources"`
}
type decisionState struct {
	ID         string `json:"id"`
	PlaybackID string `json:"playback_id"`
	ClientID   string `json:"client_id"`
	Plan       struct {
		Kind string `json:"kind"`
	} `json:"plan_decision"`
	Format           string `json:"format"`
	TranscoderActive bool   `json:"transcoder_active"`
	Transcoder       *struct {
		VideoCopy bool `json:"videoCopy"`
		AudioCopy bool `json:"audioCopy"`
	} `json:"transcoder_decision"`
}
type decisionVerify struct {
	ReleaseID string `json:"release_id"`
	SourceID  string `json:"source_id"`
	Expected  string `json:"expected_sha256"`
	Actual    string `json:"actual_sha256"`
	Match     bool   `json:"match"`
}
type decisionLabServer struct {
	base, origin, device, token, user string
	client                            *http.Client
}

func decisionLabSettings(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid credentials file")
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		} else if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			var e error
			v, e = strconv.Unquote(v)
			if e != nil {
				return nil, fmt.Errorf("invalid quoted credentials entry")
			}
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, scanner.Err()
}
func (s *decisionLabServer) request(method, path string, body any, out any) error {
	var data io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.base+path, data)
	if err != nil {
		return fmt.Errorf("invalid request")
	}
	auth := `MediaBrowser Client="EtherDecisionTest", Device="Scorecard", DeviceId="` + s.device + `", Version="1"`
	if s.token != "" {
		auth += `, Token="` + s.token + `"`
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if out != nil {
		if err = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(out); err != nil {
			return fmt.Errorf("invalid JSON response")
		}
	}
	return nil
}
func (s *decisionLabServer) login(user, password string) error {
	var result struct {
		AccessToken string
		User        struct {
			ID string `json:"Id"`
		}
	}
	if user == "" || password == "" {
		return fmt.Errorf("missing credentials")
	}
	if err := s.request("POST", "/Users/AuthenticateByName", map[string]string{"Username": user, "Pw": password}, &result); err != nil {
		return err
	}
	if result.AccessToken == "" || result.User.ID == "" {
		return fmt.Errorf("invalid sign-in")
	}
	s.token = result.AccessToken
	s.user = result.User.ID
	return nil
}
func (s *decisionLabServer) logout() error {
	return s.request("POST", "/Sessions/Logout", map[string]any{}, nil)
}
func (s *decisionLabServer) stop(session, item string) error {
	return s.request("POST", "/Sessions/Playing/Stopped", map[string]any{"PlaySessionId": session, "ItemId": item}, nil)
}
func (s *decisionLabServer) items() ([]decisionItem, error) {
	var result struct {
		Items []decisionItem `json:"Items"`
	}
	err := s.request("GET", "/Items?Recursive=true&IncludeItemTypes=Movie&Fields=ProviderIds,MediaSources,Path&Limit=1000", nil, &result)
	return result.Items, err
}
func (s *decisionLabServer) playback(item, source string, profile json.RawMessage) (decisionReply, error) {
	var result decisionReply
	err := s.request("POST", "/Items/"+url.PathEscape(item)+"/PlaybackInfo?IsPlayback=true", map[string]any{"UserId": s.user, "MediaSourceId": source, "DeviceProfile": profile, "IsPlayback": true}, &result)
	return result, err
}
func (s *decisionLabServer) broker(id string) (decisionState, error) {
	var rows []decisionState
	if err := s.request("GET", "/__scorecard/broker", nil, &rows); err != nil {
		return decisionState{}, err
	}
	var found []decisionState
	for _, row := range rows {
		if row.ID == id {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		return decisionState{}, fmt.Errorf("broker session missing or ambiguous")
	}
	return found[0], nil
}
func (s *decisionLabServer) verify(id string) (decisionVerify, error) {
	var result decisionVerify
	err := s.request("GET", "/__scorecard/verify?id="+url.QueryEscape(id), nil, &result)
	return result, err
}
func (s *decisionLabServer) readHLS(path string) error {
	base, err := url.Parse(s.base)
	if err != nil {
		return err
	}
	next, err := s.streamURL(path)
	if err != nil {
		return fmt.Errorf("invalid stream URL")
	}
	for depth := 0; depth < 4; depth++ {
		if next.Scheme != base.Scheme || next.Host != base.Host {
			return fmt.Errorf("stream URL escaped fixture")
		}
		req, err := http.NewRequest("GET", next.String(), nil)
		if err != nil {
			return fmt.Errorf("invalid stream request")
		}
		response, err := s.client.Do(req)
		if err != nil {
			return fmt.Errorf("stream request failed")
		}
		data, e := io.ReadAll(io.LimitReader(response.Body, 32<<20))
		response.Body.Close()
		if e != nil || response.StatusCode != 200 {
			return fmt.Errorf("stream response unavailable")
		}
		if len(data) == 0 || len(data) == 32<<20 {
			return fmt.Errorf("empty or oversized stream response")
		}
		if !bytes.HasPrefix(data, []byte("#EXTM3U")) {
			if path := os.Getenv("SCORECARD_SEGMENT_EVIDENCE"); path != "" {
				if err := os.WriteFile(path, data, 0600); err != nil {
					return fmt.Errorf("cannot retain segment")
				}
			}
			return nil
		}
		found := ""
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				found = line
				break
			}
		}
		if found == "" {
			return fmt.Errorf("empty HLS playlist")
		}
		next, err = next.Parse(found)
		if err != nil {
			return fmt.Errorf("invalid HLS URI")
		}
	}
	return fmt.Errorf("HLS playlist nesting exceeded")
}

// readDirect sends the actual client-style static request with authentication
// in a header, never in the URL. Only a bounded response prefix is read.
func (s *decisionLabServer) readDirect(profile, item string, offer PlaybackOffer, session string) error {
	pathID := item
	if profile == "jellycon-default" {
		pathID = offer.ID
	}
	q := url.Values{"static": {"true"}, "PlaySessionId": {session}, "MediaSourceId": {offer.ID}}
	requestURL := s.base + "/Videos/" + url.PathEscape(pathID) + "/stream?" + q.Encode()
	if profile == "streamyfin-android-exoplayer" && offer.IsRemote && offer.Protocol == "Http" && offer.Path != "" {
		parsed, err := url.Parse(offer.Path)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" {
			return fmt.Errorf("remote direct path is not absolute")
		}
		translated, err := s.streamURL(offer.Path)
		if err != nil {
			return fmt.Errorf("remote direct path escaped fixture")
		}
		requestURL = translated.String()
	}
	if strings.Contains(requestURL, s.token) {
		return fmt.Errorf("account credential appeared in direct URL")
	}
	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return fmt.Errorf("invalid direct request")
	}
	req.Header.Set("Authorization", `MediaBrowser Client="EtherDecisionTest", Device="Scorecard", DeviceId="`+s.device+`", Version="1", Token="`+s.token+`"`)
	req.Header.Set("Range", "bytes=0-65535")
	response, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("direct request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("direct request HTTP %d", response.StatusCode)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 65536))
	if err != nil {
		return fmt.Errorf("direct read failed")
	}
	return nil
}

func exactDecisionSource(offers []PlaybackOffer, id string) (PlaybackOffer, bool) {
	var result PlaybackOffer
	count := 0
	for _, offer := range offers {
		if offer.ID == id {
			result = offer
			count++
		}
	}
	return result, count == 1
}

// streamURL validates the actual fixture origin, then maps only that origin to
// the local SSH forward. The original path and query are never changed.
func (s *decisionLabServer) streamURL(raw string) (*url.URL, error) {
	base, err := url.Parse(s.base)
	if err != nil {
		return nil, err
	}
	original, err := url.Parse(raw)
	if err != nil || original.User != nil {
		return nil, fmt.Errorf("invalid stream URL")
	}
	if !original.IsAbs() {
		if original.Host != "" {
			return nil, fmt.Errorf("network-path URL refused")
		}
		return base.ResolveReference(original), nil
	}
	if original.Scheme == base.Scheme && original.Host == base.Host {
		return original, nil
	}
	if s.origin == "" {
		return nil, fmt.Errorf("unexpected stream origin")
	}
	origin, err := url.Parse(s.origin)
	if err != nil || origin.Scheme != "http" || origin.Hostname() != "127.0.0.1" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || original.Scheme != origin.Scheme || original.Host != origin.Host {
		return nil, fmt.Errorf("unexpected stream origin")
	}
	original.Scheme, original.Host = base.Scheme, base.Host
	return original, nil
}

func TestDecisionStreamURLKeepsForwardedFixtureBoundary(t *testing.T) {
	s := decisionLabServer{base: "http://127.0.0.1:18196", origin: "http://127.0.0.1:18096"}
	for _, tc := range []struct{ input, want string }{
		{"/sessions/media/cap/file?x=1", "http://127.0.0.1:18196/sessions/media/cap/file?x=1"},
		{"http://127.0.0.1:18096/sessions/media/cap/file?x=1", "http://127.0.0.1:18196/sessions/media/cap/file?x=1"},
		{"http://attacker.invalid/file", ""},
		{"//attacker.invalid/file", ""},
		{"http://user@127.0.0.1:18096/file", ""},
	} {
		got, err := s.streamURL(tc.input)
		if tc.want == "" {
			if err == nil {
				t.Errorf("accepted %q", tc.input)
			}
			continue
		}
		if err != nil || got.String() != tc.want {
			t.Errorf("%q: %v %v", tc.input, got, err)
		}
	}
}

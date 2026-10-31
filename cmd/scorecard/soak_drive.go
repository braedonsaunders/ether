package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type soakTiming struct {
	Steady   time.Duration
	Fault    time.Duration
	Followup time.Duration
	Cleanup  time.Duration
	Sample   time.Duration
}

func profileTiming(profile string) (soakTiming, error) {
	switch profile {
	case "shakedown":
		return soakTiming{Steady: 8 * time.Minute, Fault: 3 * time.Minute, Followup: 5*time.Minute + 20*time.Second, Cleanup: 5*time.Minute + 15*time.Second, Sample: 10 * time.Second}, nil
	case "sustained":
		return soakTiming{Steady: 135 * time.Minute, Fault: 3 * time.Minute, Followup: 5*time.Minute + 20*time.Second, Cleanup: 5*time.Minute + 15*time.Second, Sample: 15 * time.Second}, nil
	default:
		return soakTiming{}, fmt.Errorf("profile must be shakedown or sustained")
	}
}

// SoakConfig selects the Ether listeners and the run clock.
type SoakConfig struct {
	Profile   string
	Build     string
	Plex      string
	Jellyfin  string
	Emby      string
	User      string
	Password  string
	PlexToken string
	Timing    soakTiming
	Workers   int
	GPU       string
	Diagnose  bool
}

type soakRun struct {
	cfg      SoakConfig
	client   *http.Client
	mu       sync.Mutex
	mbGate   sync.Mutex
	pause    atomic.Bool
	manifest SoakManifest
	expected atomic.Int32
	plex     *plexClient
	jellyfin *mbClient
	emby     *mbClient
}

// RunSoak drives mixed Plex, Jellyfin and Emby load, injects faults, and
// returns the manifest. The caller evaluates it with EvaluateSoak.
func RunSoak(ctx context.Context, cfg SoakConfig) (SoakManifest, error) {
	if cfg.Workers < 1 {
		cfg.Workers = 2
	}
	if cfg.Timing.Sample <= 0 || cfg.Timing.Sample > 30*time.Second {
		return SoakManifest{}, fmt.Errorf("sample interval must be 1ms..30s")
	}
	if cfg.GPU == "" {
		cfg.GPU = "no_device"
	}
	run := &soakRun{
		cfg:      cfg,
		client:   &http.Client{Timeout: 20 * time.Second},
		manifest: SoakManifest{Profile: cfg.Profile, Build: cfg.Build, Started: time.Now(), GPU: cfg.GPU},
	}
	run.plex = &plexClient{base: strings.TrimRight(cfg.Plex, "/"), token: cfg.PlexToken, http: run.client}
	run.jellyfin = &mbClient{base: strings.TrimRight(cfg.Jellyfin, "/"), user: cfg.User, password: cfg.Password, http: run.client, device: "soak-jellyfin"}
	run.emby = &mbClient{base: strings.TrimRight(cfg.Emby, "/"), user: cfg.User, password: cfg.Password, http: run.client, device: "soak-emby"}
	if err := run.plex.discover(ctx); err != nil {
		return run.manifest, fmt.Errorf("plex discover: %w", err)
	}
	if err := run.jellyfin.login(ctx); err != nil {
		return run.manifest, fmt.Errorf("jellyfin login: %w", err)
	}
	if err := run.emby.login(ctx); err != nil {
		return run.manifest, fmt.Errorf("emby login: %w", err)
	}
	if err := run.jellyfin.discover(ctx); err != nil {
		return run.manifest, fmt.Errorf("jellyfin discover: %w", err)
	}
	if err := run.emby.discover(ctx); err != nil {
		return run.manifest, fmt.Errorf("emby discover: %w", err)
	}
	if cfg.Diagnose {
		return run.manifest, run.diagnose(ctx)
	}
	if err := run.sample(ctx); err != nil {
		return run.manifest, err
	}
	run.mu.Lock()
	run.manifest.Baseline = run.manifest.Samples[0]
	run.mu.Unlock()
	sampleCtx, stopSamples := context.WithCancel(ctx)
	defer stopSamples()
	go run.sampleLoop(sampleCtx)

	loadCtx, stopLoad := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			run.worker(loadCtx, fmt.Sprintf("w%d", n))
		}(i)
	}
	if err := sleep(loadCtx, cfg.Timing.Steady); err != nil {
		stopLoad()
		wg.Wait()
		return run.snapshot(), err
	}
	if err := run.faults(loadCtx); err != nil {
		stopLoad()
		wg.Wait()
		return run.snapshot(), err
	}
	stopLoad()
	wg.Wait()
	run.manifest.FaultEnd = time.Now()
	for i, client := range []string{"recover-a", "recover-b", "recover-c"} {
		if err := run.operate(ctx, "plex", "direct", client); err != nil && ctx.Err() != nil {
			return run.snapshot(), err
		}
		_ = i
	}
	recovered := time.Now()
	followCtx, stopFollow := context.WithCancel(ctx)
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			run.worker(followCtx, fmt.Sprintf("f%d", n))
		}(i)
	}
	followFor := cfg.Timing.Followup
	if cfg.Timing.Followup >= 5*time.Minute {
		if deadline := recovered.Add(5*time.Minute + 20*time.Second); time.Until(deadline) > followFor {
			followFor = time.Until(deadline)
		}
	}
	if err := sleep(followCtx, followFor); err != nil {
		stopFollow()
		wg.Wait()
		return run.snapshot(), err
	}
	stopFollow()
	wg.Wait()
	stopped := time.Now()
	run.mu.Lock()
	run.manifest.FollowupEnd = stopped
	run.manifest.LoadStopped = stopped
	run.mu.Unlock()
	if err := sleep(ctx, cfg.Timing.Cleanup); err != nil {
		return run.snapshot(), err
	}
	stopSamples()
	return run.snapshot(), nil
}

func (run *soakRun) snapshot() SoakManifest {
	run.mu.Lock()
	defer run.mu.Unlock()
	out := run.manifest
	out.Ops = append([]SoakOp(nil), run.manifest.Ops...)
	out.Samples = append([]SoakSample(nil), run.manifest.Samples...)
	out.Faults = append([]SoakFault(nil), run.manifest.Faults...)
	return out
}

func (run *soakRun) sampleLoop(ctx context.Context) {
	ticker := time.NewTicker(run.cfg.Timing.Sample)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = run.sample(context.Background())
			return
		case <-ticker.C:
			_ = run.sample(ctx)
		}
	}
}

func (run *soakRun) sample(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, run.cfg.Jellyfin+"/__scorecard/telemetry", nil)
	if err != nil {
		return err
	}
	resp, err := run.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var sample SoakSample
	if err := json.NewDecoder(resp.Body).Decode(&sample); err != nil {
		return err
	}
	if sample.At.IsZero() {
		sample.At = time.Now()
	}
	if sample.GPU == "" {
		sample.GPU = run.cfg.GPU
	}
	run.mu.Lock()
	run.manifest.Samples = append(run.manifest.Samples, sample)
	run.mu.Unlock()
	return nil
}

func (run *soakRun) worker(ctx context.Context, client string) {
	protocols := []string{"plex", "plex", "plex", "plex", "plex", "plex", "plex", "jellyfin", "jellyfin", "emby"}
	cursor := map[string]int{}
	kinds := []string{"browse", "direct", "remux", "transcode"}
	for i := 0; ctx.Err() == nil; i++ {
		if run.pause.Load() {
			if err := sleep(ctx, 50*time.Millisecond); err != nil {
				return
			}
			continue
		}
		protocol := protocols[i%len(protocols)]
		kind := kinds[cursor[protocol]%len(kinds)]
		cursor[protocol]++
		if err := run.operate(ctx, protocol, kind, client); err != nil && ctx.Err() == nil {
			_ = sleep(ctx, 100*time.Millisecond)
		}
	}
}

func (run *soakRun) diagnose(ctx context.Context) error {
	steps := []struct{ protocol, kind string }{
		{"plex", "browse"}, {"plex", "direct"}, {"plex", "remux"}, {"plex", "transcode"},
		{"jellyfin", "browse"}, {"jellyfin", "direct"}, {"jellyfin", "remux"}, {"jellyfin", "transcode"},
		{"emby", "browse"}, {"emby", "direct"}, {"emby", "remux"}, {"emby", "transcode"},
	}
	failed := 0
	for _, step := range steps {
		delivery, err := run.one(ctx, step.protocol, step.kind, "diagnose")
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "diag %s %s: %v\n", step.protocol, step.kind, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "diag %s %s: %s\n", step.protocol, step.kind, delivery)
	}
	fmt.Fprintf(os.Stderr, "diag plex part %s rating %s\n", run.plex.partKey, run.plex.ratingKey)
	run.plex.explain(ctx)
	fmt.Fprintf(os.Stderr, "diag jellyfin item %s source %s\n", run.jellyfin.item, run.jellyfin.source)
	loadCtx, stopLoad := context.WithTimeout(ctx, 25*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			run.worker(loadCtx, fmt.Sprintf("w%d", n))
		}(i)
	}
	wg.Wait()
	stopLoad()
	seen := map[string]int{}
	run.mu.Lock()
	for _, op := range run.manifest.Ops {
		if op.Success || op.Error == "" {
			continue
		}
		seen[op.Protocol+" "+op.Kind+": "+op.Error]++
	}
	run.mu.Unlock()
	for msg, n := range seen {
		fmt.Fprintf(os.Stderr, "diag load %d %s\n", n, msg)
	}
	if failed > 0 {
		return fmt.Errorf("diagnose found %d failures", failed)
	}
	return nil
}

func (run *soakRun) one(ctx context.Context, protocol, kind, client string) (string, error) {
	switch protocol {
	case "plex":
		return run.plex.do(ctx, client, kind)
	case "jellyfin":
		return run.jellyfin.do(ctx, client, kind)
	case "emby":
		return run.emby.do(ctx, client, kind)
	default:
		return "", fmt.Errorf("unknown protocol")
	}
}

func (run *soakRun) operate(ctx context.Context, protocol, kind, client string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if protocol == "jellyfin" || protocol == "emby" {
		run.mbGate.Lock()
		defer run.mbGate.Unlock()
	}
	overlapped := run.expected.Load() > 0
	delivery, err := run.one(ctx, protocol, kind, client)
	if ctx.Err() != nil && err != nil {
		return ctx.Err()
	}
	op := SoakOp{At: time.Now(), Protocol: protocol, Kind: kind, Delivery: delivery, Client: client, Success: err == nil && delivery != "" || (kind == "browse" && err == nil)}
	if kind != "browse" {
		op.Verified = err == nil && (delivery == "direct" || delivery == "hls" || delivery == "remux")
		op.Success = op.Verified
	} else {
		op.Verified = err == nil
		op.Success = err == nil
		op.Delivery = "browse"
	}
	if !op.Success && (overlapped || run.expected.Load() > 0) {
		op.ExpectedFailure = true
	}
	if err != nil {
		op.Error = snippet([]byte(err.Error()))
	}
	run.mu.Lock()
	run.manifest.Ops = append(run.manifest.Ops, op)
	run.mu.Unlock()
	return err
}

func (run *soakRun) faults(ctx context.Context) error {
	steps := []struct {
		action string
		hold   time.Duration
	}{
		{"source_outage", 20 * time.Second},
		{"slow_upstream", 20 * time.Second},
		{"encoder_crash", 15 * time.Second},
		{"encoder_kill", 15 * time.Second},
		{"db_stall", 8 * time.Second},
	}
	var sum time.Duration
	for _, step := range steps {
		sum += step.hold
	}
	if run.cfg.Timing.Fault > 0 && run.cfg.Timing.Fault < sum {
		for i := range steps {
			steps[i].hold = time.Duration(float64(steps[i].hold) * float64(run.cfg.Timing.Fault) / float64(sum))
			if steps[i].hold < 20*time.Millisecond {
				steps[i].hold = 20 * time.Millisecond
			}
		}
	}
	for _, step := range steps {
		if err := run.faultStep(ctx, step.action, step.hold); err != nil {
			return err
		}
	}
	_, err := run.postFault(ctx, "clear", 0, 0)
	return err
}

func (run *soakRun) faultStep(ctx context.Context, action string, hold time.Duration) error {
	run.expected.Add(1)
	defer run.expected.Add(-1)
	delay, durationMS := 0, int(hold/time.Millisecond)
	switch action {
	case "slow_upstream":
		delay = 200
		if hold < 200*time.Millisecond {
			delay = durationMS
		}
	case "db_stall":
		durationMS = int((hold / 2) / time.Millisecond)
		if durationMS < 1 {
			durationMS = 1
		}
		if durationMS > 4000 {
			durationMS = 4000
		}
	}
	if action == "encoder_crash" || action == "encoder_kill" {
		run.pause.Store(true)
		defer run.pause.Store(false)
		deadline := time.Now().Add(20 * time.Second)
		for run.ffmpegNow(ctx) > 0 && time.Now().Before(deadline) {
			if err := sleep(ctx, 200*time.Millisecond); err != nil {
				return err
			}
		}
		go run.operate(ctx, "plex", "transcode", "encoder-hold")
		_ = run.waitFFmpeg(ctx, minDuration(hold*2/3, 8*time.Second))
	}
	fault, err := run.postFault(ctx, action, delay, durationMS)
	if err != nil && (action == "encoder_crash" || action == "encoder_kill") {
		go run.operate(ctx, "plex", "transcode", "encoder-retry")
		_ = run.waitFFmpeg(ctx, minDuration(hold*2/3, 8*time.Second))
		fault, err = run.postFault(ctx, action, delay, durationMS)
	}
	run.mu.Lock()
	run.manifest.Faults = append(run.manifest.Faults, fault)
	run.mu.Unlock()
	if err != nil {
		return err
	}
	if err := sleep(ctx, hold); err != nil {
		return err
	}
	_, err = run.postFault(ctx, "clear", 0, 0)
	return err
}

func (run *soakRun) waitFFmpeg(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	deadline := time.Now().Add(d)
	for {
		if run.ffmpegNow(ctx) > 0 {
			return true
		}
		remain := time.Until(deadline)
		if remain <= 0 || ctx.Err() != nil {
			return false
		}
		step := 200 * time.Millisecond
		if remain < step {
			step = remain
		}
		if err := sleep(ctx, step); err != nil {
			return false
		}
	}
}

func (run *soakRun) ffmpegNow(ctx context.Context) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, run.cfg.Jellyfin+"/__scorecard/telemetry", nil)
	if err != nil {
		return 0
	}
	resp, err := run.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var sample SoakSample
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&sample); err != nil {
		return 0
	}
	return sample.FFmpeg
}

func (run *soakRun) postFault(ctx context.Context, action string, delayMS, durationMS int) (SoakFault, error) {
	body, _ := json.Marshal(map[string]any{"action": action, "delay_ms": delayMS, "duration_ms": durationMS})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, run.cfg.Jellyfin+"/__scorecard/fault", bytes.NewReader(body))
	if err != nil {
		return SoakFault{Action: action, At: time.Now()}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := run.client.Do(req)
	if err != nil {
		return SoakFault{Action: action, At: time.Now()}, err
	}
	defer resp.Body.Close()
	var parsed struct {
		OK       bool `json:"ok"`
		Signaled int  `json:"signaled"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed)
	fault := SoakFault{Action: action, At: time.Now(), OK: resp.StatusCode == http.StatusOK && parsed.OK, Signaled: parsed.Signaled}
	if action == "encoder_crash" || action == "encoder_kill" {
		fault.OK = fault.OK && fault.Signaled > 0
	}
	if !fault.OK {
		return fault, fmt.Errorf("%s was rejected", action)
	}
	return fault, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func mediaDelivery(kind string, status int, body []byte) (string, bool) {
	if bytes.HasPrefix(body, []byte("#EXTM3U")) {
		return "hls", true
	}
	if (status == http.StatusOK || status == http.StatusPartialContent) && mediaBytes(body) {
		if kind == "remux" || kind == "transcode" {
			return "direct", true
		}
		return "direct", true
	}
	return "", false
}

func mediaBytes(body []byte) bool {
	if len(body) >= 8 && bytes.Equal(body[4:8], []byte("ftyp")) {
		return true
	}
	if bytes.HasPrefix(body, []byte{0x1a, 0x45, 0xdf, 0xa3}) || (len(body) > 0 && body[0] == 0x47) {
		return true
	}
	return false
}

type plexClient struct {
	base, token, ratingKey, partKey string
	http                            *http.Client
}

func (p *plexClient) discover(ctx context.Context) error {
	body, status, err := p.req(ctx, http.MethodGet, "/library/sections", "sections", nil, "")
	if err != nil || status != 200 {
		return fmt.Errorf("sections %d %v", status, err)
	}
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key  string `json:"key"`
				Type string `json:"type"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &sections); err != nil {
		return err
	}
	key := ""
	for _, dir := range sections.MediaContainer.Directory {
		if dir.Type == "movie" || dir.Type == "" {
			key = dir.Key
			if dir.Type == "movie" {
				break
			}
		}
	}
	if key == "" {
		return fmt.Errorf("no plex library")
	}
	body, status, err = p.req(ctx, http.MethodGet, "/library/sections/"+url.PathEscape(key)+"/all", "sections", nil, "")
	if err != nil || status != 200 {
		return fmt.Errorf("library %d %v", status, err)
	}
	var page struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.MediaContainer.Metadata) == 0 {
		return fmt.Errorf("plex library is empty")
	}
	var last error
	for _, item := range page.MediaContainer.Metadata {
		if item.RatingKey == "" {
			continue
		}
		part, err := p.partFor(ctx, item.RatingKey)
		if err != nil {
			last = err
			continue
		}
		data, status, err := p.req(ctx, http.MethodGet, part, "sections", nil, "bytes=0-65535")
		p.ratingKey = item.RatingKey
		p.partKey = part
		p.stop(ctx, "sections")
		if err != nil {
			last = err
			continue
		}
		if _, ok := mediaDelivery("direct", status, data); !ok {
			last = fmt.Errorf("rating %s status %d %s", item.RatingKey, status, snippet(data))
			continue
		}
		return nil
	}
	if last == nil {
		last = fmt.Errorf("plex library is empty")
	}
	return fmt.Errorf("no playable plex movie: %w", last)
}

func (p *plexClient) partFor(ctx context.Context, ratingKey string) (string, error) {
	body, status, err := p.req(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(ratingKey), "meta", nil, "")
	if err != nil || status != 200 {
		return "", fmt.Errorf("metadata %d %v", status, err)
	}
	var meta struct {
		MediaContainer struct {
			Metadata []struct {
				Media []struct {
					Part []struct {
						Key string `json:"key"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return "", err
	}
	for _, item := range meta.MediaContainer.Metadata {
		for _, media := range item.Media {
			for _, part := range media.Part {
				if part.Key != "" {
					return part.Key, nil
				}
			}
		}
	}
	return "", fmt.Errorf("plex item has no part")
}

func (p *plexClient) stop(ctx context.Context, client string) {
	q := url.Values{}
	q.Set("ratingKey", p.ratingKey)
	q.Set("key", "/library/metadata/"+p.ratingKey)
	q.Set("state", "stopped")
	q.Set("time", "1000")
	q.Set("duration", "1000")
	_, _, _ = p.req(ctx, http.MethodGet, "/:/timeline?"+q.Encode(), client, nil, "")
}

func (p *plexClient) explain(ctx context.Context) {
	q := url.Values{}
	q.Set("path", "/library/metadata/"+p.ratingKey)
	q.Set("protocol", "http")
	q.Set("directPlay", "1")
	q.Set("directStream", "1")
	q.Set("session", "diagnose-decision")
	body, status, err := p.req(ctx, http.MethodGet, "/video/:/transcode/universal/decision?"+q.Encode(), "diagnose", nil, "")
	window := body
	if i := bytes.Index(body, []byte("directPlayDecisionText")); i >= 0 {
		end := i + 280
		if end > len(body) {
			end = len(body)
		}
		window = body[i:end]
	}
	fmt.Fprintf(os.Stderr, "diag plex decision %d %v %s\n", status, err, snippet(window))
}

func (p *plexClient) do(ctx context.Context, client, kind string) (string, error) {
	if kind == "browse" {
		_, status, err := p.req(ctx, http.MethodGet, "/library/sections", client, nil, "")
		if err != nil || status != 200 {
			return "", fmt.Errorf("browse %d %v", status, err)
		}
		return "browse", nil
	}
	if kind == "direct" {
		defer p.stop(context.Background(), client)
		body, status, err := p.req(ctx, http.MethodGet, p.partKey, client, nil, "bytes=0-65535")
		if err != nil {
			return "", err
		}
		delivery, ok := mediaDelivery(kind, status, body)
		if !ok {
			return delivery, fmt.Errorf("direct status %d %s", status, snippet(body))
		}
		return delivery, nil
	}
	session := client + "-" + kind
	q := url.Values{}
	q.Set("path", "/library/metadata/"+p.ratingKey)
	q.Set("session", session)
	q.Set("protocol", "hls")
	q.Set("directPlay", "0")
	if kind == "remux" {
		q.Set("directStream", "1")
	} else {
		q.Set("directStream", "0")
		q.Set("videoBitrate", "400")
		q.Set("maxVideoBitrate", "400")
	}
	startPath := "/video/:/transcode/universal/start.m3u8?" + q.Encode()
	defer p.req(context.Background(), http.MethodGet, "/video/:/transcode/universal/stop?session="+url.QueryEscape(session), client, nil, "")
	body, status, err := p.req(ctx, http.MethodGet, startPath, client, nil, "")
	if err != nil || status != 200 {
		return "", fmt.Errorf("start %d %v %s", status, err, snippet(body))
	}
	delivery, ok := mediaDelivery(kind, status, body)
	if !ok {
		return "", fmt.Errorf("start body was not media %s", snippet(body))
	}
	if bytes.HasPrefix(body, []byte("#EXTM3U")) {
		if err := readPlaylist(ctx, p.base+startPath, string(body), func(next string) ([]byte, int, error) {
			return p.req(ctx, http.MethodGet, next, client, nil, "")
		}); err != nil {
			return "", err
		}
	}
	return delivery, nil
}

func (p *plexClient) req(ctx context.Context, method, path, client string, body []byte, rangeHeader string) ([]byte, int, error) {
	target := p.base + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		target = path
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Plex-Token", p.token)
	req.Header.Set("X-Plex-Client-Identifier", client)
	req.Header.Set("X-Plex-Product", "Plex Web")
	req.Header.Set("X-Plex-Platform", "Chrome")
	req.Header.Set("X-Plex-Version", "4.147.0")
	req.Header.Set("X-Plex-Device-Name", "soak")
	req.Header.Set("Accept", "application/json")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
		req.Header.Del("Accept")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return data, resp.StatusCode, err
}

type mbClient struct {
	base, user, password, token, userID, item, source, device string
	http                                                      *http.Client
}

func (m *mbClient) login(ctx context.Context) error {
	payload, _ := json.Marshal(map[string]string{"Username": m.user, "Pw": m.password})
	body, status, err := m.req(ctx, http.MethodPost, "/Users/AuthenticateByName", payload, "", "")
	if err != nil || status != 200 {
		return fmt.Errorf("login %d %v", status, err)
	}
	var parsed struct {
		AccessToken string
		User        struct{ Id string }
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AccessToken == "" || parsed.User.Id == "" {
		return fmt.Errorf("login incomplete")
	}
	m.token = parsed.AccessToken
	m.userID = parsed.User.Id
	return nil
}

func (m *mbClient) discover(ctx context.Context) error {
	body, status, err := m.req(ctx, http.MethodGet, "/Users/"+url.PathEscape(m.userID)+"/Items?Recursive=true&IncludeItemTypes=Movie&Limit=5", nil, "", "")
	if err != nil || status != 200 {
		return fmt.Errorf("items %d %v", status, err)
	}
	var page struct {
		Items []struct{ Id string }
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Items) == 0 || page.Items[0].Id == "" {
		return fmt.Errorf("no movie")
	}
	m.item = page.Items[0].Id
	body, status, err = m.req(ctx, http.MethodGet, "/Items/"+url.PathEscape(m.item), nil, "", "")
	if err != nil || status != 200 {
		return fmt.Errorf("detail %d %v", status, err)
	}
	var detail struct {
		MediaSources []struct{ Id string }
	}
	if err := json.Unmarshal(body, &detail); err != nil || len(detail.MediaSources) == 0 || detail.MediaSources[0].Id == "" {
		return fmt.Errorf("no media source")
	}
	m.source = detail.MediaSources[0].Id
	return nil
}

func (m *mbClient) do(ctx context.Context, client, kind string) (string, error) {
	if kind == "browse" {
		_, status, err := m.req(ctx, http.MethodGet, "/Users/"+url.PathEscape(m.userID)+"/Items?Recursive=true&IncludeItemTypes=Movie&Limit=5", nil, client, "")
		if err != nil || status != 200 {
			return "", fmt.Errorf("browse %d %v", status, err)
		}
		return "browse", nil
	}
	profile := directProfile()
	if kind == "remux" {
		profile = remuxProfile()
	}
	if kind == "transcode" {
		profile = transcodeProfile()
	}
	payload, _ := json.Marshal(map[string]any{
		"IsPlayback": true, "EnableDirectPlay": true, "EnableDirectStream": true, "EnableTranscoding": true,
		"DeviceProfile": profile, "MediaSourceId": m.source,
	})
	body, status, err := m.req(ctx, http.MethodPost, "/Items/"+url.PathEscape(m.item)+"/PlaybackInfo", payload, client, "")
	if err != nil || status != 200 {
		return "", fmt.Errorf("playback %d %v %s", status, err, snippet(body))
	}
	var info struct {
		PlaySessionId string
		MediaSources  []struct {
			Id             string
			TranscodingUrl string
		}
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", err
	}
	session := info.PlaySessionId
	defer m.req(context.Background(), http.MethodPost, "/Sessions/Playing/Stopped", mustJSON(map[string]string{"PlaySessionId": session, "ItemId": m.item}), client, "")
	chosen := ""
	for _, source := range info.MediaSources {
		if source.TranscodingUrl != "" {
			chosen = source.TranscodingUrl
		}
	}
	if chosen != "" {
		current := chosen
		if !strings.Contains(current, "://") {
			current = m.base + chosen
		}
		if err := readPlaylist(ctx, current, "", func(next string) ([]byte, int, error) {
			return m.req(ctx, http.MethodGet, next, nil, client, "")
		}); err != nil {
			return "", err
		}
		return "hls", nil
	}
	q := url.Values{"static": {"true"}, "PlaySessionId": {session}, "MediaSourceId": {m.source}}
	data, status, err := m.req(ctx, http.MethodGet, "/Videos/"+url.PathEscape(m.item)+"/stream?"+q.Encode(), nil, client, "bytes=0-65535")
	if err != nil {
		return "", err
	}
	delivery, ok := mediaDelivery(kind, status, data)
	if !ok {
		return "", fmt.Errorf("stream status %d %s", status, snippet(data))
	}
	return delivery, nil
}

func readPlaylist(ctx context.Context, current, already string, get func(string) ([]byte, int, error)) error {
	for depth := 0; depth < 4; depth++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		data := []byte(already)
		status := http.StatusOK
		var err error
		if depth > 0 || already == "" {
			data, status, err = get(current)
			if err != nil || status != http.StatusOK || len(data) == 0 {
				return fmt.Errorf("hls %d %v", status, err)
			}
		}
		if !bytes.HasPrefix(data, []byte("#EXTM3U")) {
			return nil
		}
		next := ""
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				next = line
				break
			}
		}
		if next == "" {
			return fmt.Errorf("empty playlist")
		}
		base, err := url.Parse(current)
		if err != nil {
			return err
		}
		ref, err := url.Parse(next)
		if err != nil {
			return err
		}
		resolved := base.ResolveReference(ref)
		if resolved.Host != base.Host {
			return fmt.Errorf("segment left the server")
		}
		current = resolved.String()
		already = ""
	}
	return fmt.Errorf("playlist too deep")
}

func (m *mbClient) req(ctx context.Context, method, path string, payload []byte, client, rangeHeader string) ([]byte, int, error) {
	target := path
	if !strings.Contains(path, "://") {
		if !strings.HasPrefix(path, "/") {
			return nil, 0, fmt.Errorf("bad path")
		}
		target = m.base + path
	} else {
		parsed, err := url.Parse(path)
		if err != nil || parsed.Host != hostOf(m.base) {
			return nil, 0, fmt.Errorf("url left the server")
		}
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	device := m.device
	if device == "" {
		device = "soak"
	}
	name := client
	if name == "" {
		name = "soak"
	}
	auth := fmt.Sprintf(`MediaBrowser Client="Soak", Device="%s", DeviceId="%s", Version="1"`, name, device)
	if m.token != "" {
		auth += fmt.Sprintf(`, Token="%s"`, m.token)
		req.Header.Set("X-Emby-Token", m.token)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return data, resp.StatusCode, err
}

func directProfile() map[string]any {
	return map[string]any{
		"MaxStreamingBitrate": 120000000,
		"DirectPlayProfiles":  []any{map[string]any{"Container": "mkv,mp4,mov", "Type": "Video", "VideoCodec": "h264,hevc", "AudioCodec": "aac,mp3,ac3"}},
		"TranscodingProfiles": []any{map[string]any{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac", "Protocol": "hls", "Context": "Streaming"}},
	}
}

func remuxProfile() map[string]any {
	return map[string]any{
		"MaxStreamingBitrate": 120000000,
		"DirectPlayProfiles":  []any{},
		"TranscodingProfiles": []any{map[string]any{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac,mp3,ac3", "Protocol": "hls", "Context": "Streaming"}},
	}
}

func transcodeProfile() map[string]any {
	return map[string]any{
		"MaxStreamingBitrate": 2000000,
		"DirectPlayProfiles":  []any{},
		"TranscodingProfiles": []any{map[string]any{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac", "Protocol": "hls", "Context": "Streaming"}},
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func snippet(body []byte) string {
	if len(body) > 160 {
		body = body[:160]
	}
	out := make([]byte, 0, len(body))
	for _, b := range body {
		if b < 32 || b > 126 {
			out = append(out, '.')
		} else {
			out = append(out, b)
		}
	}
	return string(out)
}

func hostOf(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return parsed.Host
}

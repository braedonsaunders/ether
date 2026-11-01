package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// copyFixture rejects links and non-regular files so a fixture cannot escape
// its disposable directory. SQLite must be closed and checkpointed first.
func copyFixture(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular fixture file: %s", path)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

// runPair fails on unapproved differences and reports incomplete coverage
// separately so callers can distinguish a clean partial run from a regression.
func runPair(ctx context.Context, baseline, candidate, seedDir string, out io.Writer) error {
	if os.Getenv("ETHER_PARITY_CLUSTER_ONLY") == "1" {
		if err := runClusterPair(ctx, baseline, candidate, seedDir, out); err != nil {
			return err
		}
		return incompleteCoverage{[]string{"only two-node cluster session probes executed"}}
	}
	subtitleOnly := os.Getenv("ETHER_PARITY_SUBTITLES_ONLY") == "1"
	if err := runPairMode(ctx, baseline, candidate, seedDir, out, subtitleOnly); err != nil {
		return err
	}
	for _, mode := range []struct{ env, description string }{
		{"ETHER_PARITY_ATTRIBUTION", "artwork probes skipped for historical attribution"},
		{"ETHER_PARITY_NOTIFICATIONS_ONLY", "only notification probes executed"},
		{"ETHER_PARITY_LIVETV_ONLY", "only live TV probes executed"},
		{"ETHER_PARITY_DOWNLOADS_ONLY", "only download probes executed"},
		{"ETHER_PARITY_TRICKPLAY_ONLY", "only trickplay probes executed"},
		{"ETHER_PARITY_SUBTITLES_ONLY", "only subtitle probes executed"},
		{"ETHER_PARITY_IPTV_ONLY", "only IPTV probes executed"},
		{"ETHER_PARITY_POLICY_COUNTERFACTUAL", "diagnostic counterfactual policy replay"},
		{"ETHER_PARITY_EXPIRY_ONLY", "only transcoder expiry probe executed"},
		{"ETHER_PARITY_INFUSE_ONLY", "only Infuse direct resume probe executed"},
	} {
		if os.Getenv(mode.env) == "1" {
			return incompleteCoverage{[]string{mode.description}}
		}
	}
	// A separately configured copy of the same seed enables the offline
	// subtitle provider without changing the primary world's responses.
	if err := runPairMode(ctx, baseline, candidate, seedDir, out, true); err != nil {
		return err
	}
	if missing := missingRoutes(); len(missing) > 0 {
		return incompleteCoverage{missing}
	}
	return nil
}

func runPairMode(ctx context.Context, baseline, candidate, seedDir string, out io.Writer, subtitleMode bool) (err error) {
	if os.Getenv("ETHER_PARITY_TRAIN_RANKER") == "1" {
		b, err := os.ReadFile(filepath.Join(seedDir, "ranker-training.json"))
		if err != nil {
			return fmt.Errorf("active-ranker training proof missing: %w", err)
		}
		var proof struct {
			Description string `json:"description"`
			Insight     struct {
				Active      bool    `json:"active"`
				Examples    int     `json:"examples"`
				Positives   int     `json:"positives"`
				AUC         float64 `json:"auc"`
				BaselineAUC float64 `json:"baselineAuc"`
			} `json:"insight"`
			HoldoutPositives int                           `json:"holdout_positives"`
			HoldoutNegatives int                           `json:"holdout_negatives"`
			HoldoutViewers   map[string]rankerHoldoutClass `json:"holdout_viewers"`
		}
		if err := json.Unmarshal(b, &proof); err != nil {
			return fmt.Errorf("invalid active-ranker training proof: %w", err)
		}
		if proof.Description == "" || !proof.Insight.Active || proof.Insight.Examples < 2000 || proof.Insight.Positives < 30 || proof.Insight.AUC < 0.55 || proof.Insight.AUC < proof.Insight.BaselineAUC+0.02 || proof.HoldoutPositives < 5 || proof.HoldoutNegatives < 20 {
			return fmt.Errorf("active-ranker training proof does not satisfy the production gate")
		}
		for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
			if counts := proof.HoldoutViewers[viewer]; counts.Positives == 0 || counts.Negatives == 0 {
				return fmt.Errorf("active-ranker training proof lacks both holdout classes for %s", viewer)
			}
		}
		fmt.Fprintf(out, "ACTIVE_RANKER_PROOF %s\n", bytes.TrimSpace(b))
	}
	dir, err := os.MkdirTemp("", "ether-plexparity-")
	if err != nil {
		return err
	}
	retainDir := retainedServerLogDir(seedDir, subtitleMode)
	defer func() {
		if retainErr := retainServerLogs(dir, retainDir); retainErr != nil && err == nil {
			err = fmt.Errorf("retain server logs: %w", retainErr)
		}
		os.RemoveAll(dir)
	}()
	tracker := &fixtureTracker{}
	tmdb := httptest.NewServer(trackedTMDBFixture(tracker))
	defer tmdb.Close()
	plexTracker := &fixtureTracker{}
	plexTV := httptest.NewServer(trackedPlexTVFixture(plexTracker))
	defer plexTV.Close()
	subtitleTracker := &fixtureTracker{}
	subtitles := httptest.NewServer(subtitleFixture(subtitleTracker))
	defer subtitles.Close()
	iptvTracker := &fixtureTracker{}
	iptvHandler, err := iptvFixture(seedDir, iptvTracker)
	if err != nil {
		return err
	}
	iptvListener, err := net.Listen("tcp", "127.0.0.1:32410")
	if err != nil {
		return fmt.Errorf("IPTV fixture port must be free: %w", err)
	}
	iptv := httptest.NewUnstartedServer(iptvHandler)
	iptv.Listener.Close()
	iptv.Listener = iptvListener
	iptv.Start()
	defer iptv.Close()
	transcripts := make([]transcript, 0, 2)
	seedFiles, err := seedManifest(seedDir)
	if err != nil {
		return fmt.Errorf("seed source manifest: %w", err)
	}
	hasDB := false
	for _, file := range seedFiles {
		if file.Path == "ether.db" {
			hasDB = true
		}
	}
	if !hasDB {
		return fmt.Errorf("seed manifest missing ether.db (including ranker model)")
	}
	for i, bin := range []string{baseline, candidate} {
		dest := filepath.Join(dir, "server")
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if err := copyFixture(seedDir, dest); err != nil {
			return err
		}
		// Validate every copied file before config.Open/Update can change any
		// bytes. Re-hash the source too, so a concurrent seed write fails closed.
		sourceNow, err := seedManifest(seedDir)
		if err != nil {
			return fmt.Errorf("seed source recheck side %d: %w", i, err)
		}
		copied, err := seedManifest(dest)
		if err != nil {
			return fmt.Errorf("seed copy manifest side %d: %w", i, err)
		}
		if err := checkSeedCopy(seedFiles, sourceNow, copied); err != nil {
			return fmt.Errorf("seed manifest side %d: %w", i, err)
		}
		receipt, err := json.Marshal(struct {
			Side   int        `json:"side"`
			Source []seedFile `json:"source"`
			Copy   []seedFile `json:"copy"`
		}{i, sourceNow, copied})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "SEED_COPY_VERIFIED %s\n", receipt)
		c, err := config.Open(dest)
		if err != nil {
			return err
		}
		addr := "127.0.0.1"
		port := 32400
		if err := c.Update(func(c *config.Config) error {
			c.Plex.Token = "parity-owner"
			c.Plex.OwnerPlexID = 1
			c.Server.BindAddress = addr
			c.Server.Port = port
			c.Cache.Dir = filepath.Join(dest, "cache")
			c.Transcode.TempDir = filepath.Join(dest, "transcode")
			if subtitleMode {
				c.OpenSubtitles.Enabled = true
				c.OpenSubtitles.APIKey = "parity-fixture"
				c.Cache.DisableSpeculation = true
			}
			return nil
		}); err != nil {
			return err
		}
		applied := c.Get()
		if applied.Plex.Token != "parity-owner" || applied.Plex.OwnerPlexID != 1 || applied.Server.BindAddress != addr || applied.Server.Port != port || applied.Cache.Dir != filepath.Join(dest, "cache") || applied.Transcode.TempDir != filepath.Join(dest, "transcode") || (subtitleMode && (!applied.OpenSubtitles.Enabled || applied.OpenSubtitles.APIKey != "parity-fixture" || !applied.Cache.DisableSpeculation)) {
			return fmt.Errorf("seed configuration mutation mismatch on side %d", i)
		}
		fmt.Fprintf(out, "SEED_CONFIG_MUTATION_VERIFIED side=%d subtitle=%t (fixture fields; values withheld)\n", i, subtitleMode)
		listener, err := net.Listen("tcp", net.JoinHostPort(addr, "32400"))
		if err != nil {
			return fmt.Errorf("parity port must be free: %w", err)
		}
		listener.Close()
		log, err := os.Create(filepath.Join(dir, fmt.Sprintf("server-%d.log", i)))
		if err != nil {
			return err
		}
		runCtx, cancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(runCtx, bin, "--data-dir", dest)
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.Env = append(os.Environ(), "ETHER_PARITY_TMDB_URL="+tmdb.URL+"/3", "ETHER_PARITY_PLEXTV_URL="+plexTV.URL, "ETHER_PARITY_SUBTITLES_URL="+subtitles.URL)
		if err = cmd.Start(); err != nil {
			cancel()
			log.Close()
			if keepErr := publishServerLog(out, i, log.Name(), retainDir); keepErr != nil {
				return fmt.Errorf("%w (retaining log: %v)", err, keepErr)
			}
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		url := "http://" + net.JoinHostPort(addr, "32400")
		client := &http.Client{Timeout: time.Second}
		deadline := time.NewTimer(120 * time.Second)
		ready := false
		for !ready {
			select {
			case err := <-done:
				done <- err
				deadline.Stop()
				cancel()
				log.Close()
				if keepErr := publishServerLog(out, i, log.Name(), retainDir); keepErr != nil {
					return fmt.Errorf("server %d exited during startup: %v (retaining log: %v)", i, err, keepErr)
				}
				return fmt.Errorf("server %d exited during startup: %v", i, err)
			case <-deadline.C:
				cancel()
				<-done
				log.Close()
				if keepErr := publishServerLog(out, i, log.Name(), retainDir); keepErr != nil {
					return fmt.Errorf("server %d startup timed out (retaining log: %v)", i, keepErr)
				}
				return fmt.Errorf("server %d startup timed out", i)
			case <-ctx.Done():
				deadline.Stop()
				cancel()
				<-done
				log.Close()
				if keepErr := publishServerLog(out, i, log.Name(), retainDir); keepErr != nil {
					return fmt.Errorf("%w (retaining log: %v)", ctx.Err(), keepErr)
				}
				return ctx.Err()
			default:
			}
			resp, e := client.Get(url + "/identity")
			if e == nil {
				resp.Body.Close()
				ready = resp.StatusCode == http.StatusOK
			}
			if !ready {
				select {
				case <-ctx.Done():
					deadline.Stop()
					cancel()
					<-done
					log.Close()
					if keepErr := publishServerLog(out, i, log.Name(), retainDir); keepErr != nil {
						return fmt.Errorf("%w (retaining log: %v)", ctx.Err(), keepErr)
					}
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		if !deadline.Stop() {
			select {
			case <-deadline.C:
			default:
			}
		}
		captureCtx := ctx
		if os.Getenv("ETHER_PARITY_EXPIRY_ONLY") == "1" {
			captureCtx = context.WithValue(ctx, serverPIDKey{}, cmd.Process.Pid)
		}
		captured, captureErr := captureCorpusMode(captureCtx, url, subtitleMode)
		cancel()
		<-done
		log.Close()
		if os.Getenv("ETHER_PARITY_CROWD_DIAG") == "1" {
			if contents, err := os.ReadFile(log.Name()); err == nil {
				for _, line := range strings.Split(string(contents), "\n") {
					if strings.HasPrefix(line, "PARITY_CROWD ") {
						fmt.Fprintf(out, "server=%d %s\n", i, line)
					}
				}
			}
		}
		if missing := tracker.failures(); len(missing) > 0 && captureErr == nil {
			captureErr = fmt.Errorf("TMDB fixture does not implement %v", missing)
		}
		if missing := plexTracker.failures(); len(missing) > 0 && captureErr == nil {
			captureErr = fmt.Errorf("Plex.tv fixture does not implement %v", missing)
		}
		if missing := subtitleTracker.failures(); len(missing) > 0 && captureErr == nil {
			captureErr = fmt.Errorf("subtitle fixture does not implement %v", missing)
		}
		if missing := iptvTracker.failures(); len(missing) > 0 && captureErr == nil {
			captureErr = fmt.Errorf("IPTV fixture does not implement %v", missing)
		}
		if captureErr != nil {
			if keepErr := publishServerLog(out, i, log.Name(), retainDir); keepErr != nil {
				return fmt.Errorf("server %d: %w (retaining log: %v)", i, captureErr, keepErr)
			}
			return fmt.Errorf("server %d: %w", i, captureErr)
		}
		if os.Getenv("ETHER_PARITY_POLICY_COUNTERFACTUAL") == "1" {
			for _, c := range captured.captures {
				if (c.viewer != "teen" && c.viewer != "child") || !(strings.HasPrefix(c.path, "/search?") || strings.HasPrefix(c.path, "/library/search?") || strings.HasPrefix(c.path, "/hubs/search") || strings.Contains(c.path, "/postplay")) {
					continue
				}
				record := struct {
					Server    int               `json:"server"`
					Viewer    string            `json:"viewer"`
					Client    string            `json:"client"`
					JSON      bool              `json:"json"`
					Path      string            `json:"path"`
					Signature responseSignature `json:"signature"`
				}{i, c.viewer, c.client, c.json, c.path, signature(c.response)}
				b, err := json.Marshal(record)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "COUNTERFACTUAL %s\n", b)
			}
		}
		transcripts = append(transcripts, captured)
	}
	n, err := diffTranscripts(transcripts[0], transcripts[1], out)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%d unapproved response differences", n)
	}
	return nil
}

// retainedServerLogDir is on the stage that outlives the disposable server
// directory. Seed contents stay untouched so copy verification still matches.
func retainedServerLogDir(seedDir string, subtitleMode bool) string {
	mode := "ordinary"
	if subtitleMode {
		mode = "subtitles"
	}
	return filepath.Join(filepath.Dir(seedDir), "retained-server-logs", filepath.Base(seedDir), mode)
}

func copyServerLog(src, retainDir string) (string, error) {
	if err := os.MkdirAll(retainDir, 0700); err != nil {
		return "", err
	}
	dst := filepath.Join(retainDir, filepath.Base(src))
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	return dst, nil
}

func retainServerLogs(tempDir, retainDir string) error {
	matches, err := filepath.Glob(filepath.Join(tempDir, "server-*.log"))
	if err != nil {
		return err
	}
	for _, src := range matches {
		if _, err := copyServerLog(src, retainDir); err != nil {
			return err
		}
	}
	return nil
}

func publishServerLog(out io.Writer, server int, src, retainDir string) error {
	kept, err := copyServerLog(src, retainDir)
	if err != nil {
		return err
	}
	if kept == "" {
		return fmt.Errorf("server log %s is missing", src)
	}
	fmt.Fprintf(out, "server %d log: %s\n", server, kept)
	return nil
}

// incompleteCoverage is a non-successful probe with no unapproved diffs.
type incompleteCoverage struct{ routes []string }

func (e incompleteCoverage) Error() string {
	return fmt.Sprintf("coverage incomplete: %d GET routes: %s", len(e.routes), strings.Join(e.routes, ", "))
}

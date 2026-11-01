package app

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/logfile"
)

func (a *App) supportDeps() diagnostics.Deps {
	return diagnostics.Deps{
		Now:          parityClock(),
		Version:      a.opts.Version,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		Container:    containerHint(),
		Secrets:      func() []string { return supportSecrets(a.cfg.Get()) },
		Settings:     func() (diagnostics.Settings, error) { return supportSettings(a.cfg.Get()), nil },
		Sessions:     a.supportSessions,
		Hardware:     a.supportHardware,
		Storage:      a.supportStorage,
		Connectivity: func() (diagnostics.ConnectivityFact, error) { return supportConnectivity(a.cfg.Get()), nil },
		LogExtract:   a.supportLogs,
	}
}

func supportSecrets(cfg config.Config) []string {
	out := config.ProtectedValues(cfg)
	if v := strings.TrimSpace(os.Getenv("ETHER_DATABASE_URL")); v != "" {
		out = append(out, v)
	}
	return out
}

func supportSettings(cfg config.Config) diagnostics.Settings {
	name, height, hdr := profileFacts(cfg, cfg.Quality.Default)
	return diagnostics.Settings{
		LogLevel:              cfg.Server.LogLevel,
		FriendlyName:          cfg.Server.FriendlyName,
		QualityDefault:        cfg.Quality.Default,
		QualityRemote:         cfg.Quality.Remote,
		ProfileName:           name,
		ProfileMaxHeight:      height,
		ProfileAllowHDR:       hdr,
		HWAccel:               cfg.Transcode.HWAccel,
		MaxTranscodes:         cfg.Transcode.MaxTranscodes,
		RemoteMaxBitrateKbps:  cfg.Transcode.RemoteMaxBitrateKbps,
		SecureConnections:     cfg.Plex.SecureConnections,
		RemoteAccess:          cfg.Plex.RemoteAccess,
		CertificateConfigured: cfg.Plex.CertFile != "" || cfg.Plex.KeyFile != "",
		CacheBudgetBytes:      cfg.Cache.BudgetBytes,
	}
}

func profileFacts(cfg config.Config, name string) (string, int, bool) {
	for _, p := range cfg.Quality.Profiles {
		if p.Name == name {
			return p.Name, p.MaxHeight, p.AllowHDR
		}
	}
	return name, 0, false
}

func (a *App) supportSessions(deviceKey string) ([]diagnostics.SessionFact, int, error) {
	if a.playback == nil {
		return nil, 0, errors.New("playback unavailable")
	}
	var matched []diagnostics.SessionFact
	omitted := 0
	for _, s := range a.playback.Sessions() {
		if s == nil {
			continue
		}
		if deviceKey == "" || s.Client.ClientID != deviceKey {
			omitted++
			continue
		}
		state := "open"
		if s.ViewOffset != nil {
			state = "reported"
		}
		matched = append(matched, diagnostics.SessionFact{
			DeviceKey: s.Client.ClientID,
			State:     state,
			Decision:  string(s.Decision),
			Title:     s.TitleName,
			Username:  s.Username,
			Bytes:     s.BytesServed,
		})
	}
	return matched, omitted, nil
}

func (a *App) supportHardware() (diagnostics.HardwareFact, error) {
	if a.transcoder == nil {
		return diagnostics.HardwareFact{}, errors.New("transcoder unavailable")
	}
	hw := a.transcoder.HWAccel()
	return diagnostics.HardwareFact{Name: hw.Name, FFmpeg: a.transcoder.Version(), Available: hw.Name != "" && hw.Name != "none"}, nil
}

func (a *App) supportStorage() (diagnostics.StorageFact, error) {
	cfg := a.cfg.Get()
	fact := diagnostics.StorageFact{CacheBudgetBytes: cfg.Cache.BudgetBytes}
	if free, err := admin.DiskFree(a.opts.DataDir); err == nil {
		fact.DiskFreeBytes = free
		fact.DiskFreeKnown = true
	}
	return fact, nil
}

func supportConnectivity(cfg config.Config) diagnostics.ConnectivityFact {
	return diagnostics.ConnectivityFact{
		RemoteAccess:          cfg.Plex.RemoteAccess,
		SecureConnections:     cfg.Plex.SecureConnections,
		CertificateConfigured: cfg.Plex.CertFile != "" || cfg.Plex.KeyFile != "",
		PublicPort:            cfg.Plex.PublicPort,
	}
}

func (a *App) supportLogs(q diagnostics.LogQuery) ([]diagnostics.LogRecord, bool, error) {
	if q.MaxLines <= 0 {
		q.MaxLines = 200
	}
	var out []diagnostics.LogRecord
	truncated := false
	add := func(rec diagnostics.LogRecord) {
		if !diagnostics.LogCorrelated(rec, q) {
			return
		}
		if len(out) >= q.MaxLines {
			truncated = true
			return
		}
		out = append(out, rec)
	}
	if a.logs != nil {
		for _, e := range a.logs.Entries(0, "", "") {
			add(diagnostics.LogRecord{Time: e.Time, Level: e.Level, Message: e.Message, Attrs: e.Attrs})
		}
	}
	path := filepath.Join(a.opts.DataDir, "logs", logfile.MainLogName)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, truncated, nil
		}
		return out, truncated, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, truncated, err
	}
	const tail = 64 << 10
	skipPartial := false
	if info.Size() > tail {
		if _, err := f.Seek(info.Size()-tail, io.SeekStart); err != nil {
			return out, truncated, err
		}
		skipPartial = true
		truncated = true
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 256<<10)
	for sc.Scan() {
		if skipPartial {
			skipPartial = false
			continue
		}
		rec, ok := diagnostics.ParseLogLine(sc.Text())
		if !ok {
			continue
		}
		add(rec)
	}
	if err := sc.Err(); err != nil {
		return out, true, err
	}
	return out, truncated, nil
}

func containerHint() string {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "yes"
	}
	return ""
}

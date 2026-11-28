package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/kindstream"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// MediaProbeCache stores and looks up probed container metadata in Ether's
// durable release_probes table (implemented by *playback.Manager).
type MediaProbeCache interface {
	CachedReleaseProbe(rel core.Release) (core.MediaInfo, bool)
	RecordReleaseProbe(ctx context.Context, rel core.Release, rawPayload []byte, info core.MediaInfo)
	CachedProbeByKeys(keys []string) (core.MediaInfo, bool)
	RecordProbeByKeys(ctx context.Context, keys []string, info core.MediaInfo)
}

const mediaProbeMaxDownloadBytes = 32 << 20

func inferSourcePattern(src core.PlayableSource) string {
	pattern := strings.ToLower(strings.TrimSpace(src.Pattern))
	if pattern != "" {
		return pattern
	}
	switch {
	case src.Container != nil:
		return kindstream.PatternContainer
	case src.Release != nil:
		return kindstream.PatternRelease
	case strings.TrimSpace(src.Path) != "" && strings.TrimSpace(src.URL) == "" && strings.TrimSpace(src.BlobRef) == "":
		return kindstream.PatternFile
	default:
		return kindstream.PatternRange
	}
}

func containerSelectorSuffix(c *core.ContainerRef) string {
	if c == nil {
		return ""
	}
	fmtHint := strings.ToLower(strings.TrimSpace(c.Format))
	member := strings.ToLower(strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(c.Member), "\\", "/"), "/"))
	if fmtHint == "" && member == "" && c.MemberIndex == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "f=%s|m=%s|", fmtHint, member)
	if c.MemberIndex != nil {
		fmt.Fprintf(&b, "mi=%d|", *c.MemberIndex)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// sourceProbeIdentity returns the optional core.Release and durable
// release_probes content keys for src. URLs and local paths are SHA-256 hashed
// so signed URLs, query parameters, and paths never land in release_probes.
// Multi-file releases scope keys by FileIndex/ExactFile/Member/Track (via
// core.Release.ScopedContentID) and by any outer ContainerRef member selector
// so different files in the same pack never collide.
func sourceProbeIdentity(pluginID string, src core.PlayableSource, defaultMIME string) (*core.Release, []string) {
	var keys []string
	add := func(k string) {
		if k == "" {
			return
		}
		for _, existing := range keys {
			if existing == k {
				return
			}
		}
		keys = append(keys, k)
	}
	if h := strings.ToLower(strings.TrimSpace(src.SHA256)); len(h) == 64 {
		add("sha256:" + pluginID + ":" + h)
	}

	pattern := inferSourcePattern(src)
	if blob := strings.TrimSpace(src.BlobRef); blob != "" && pattern != kindstream.PatternContainer && pattern != kindstream.PatternFile {
		add("blob:" + pluginID + ":" + blob)
		return nil, keys
	}

	switch pattern {
	case kindstream.PatternRelease:
		if src.Release == nil {
			return nil, keys
		}
		rel, err := releaseFromSource(src, "", defaultMIME)
		if err != nil {
			return nil, keys
		}
		if ctrSuffix := containerSelectorSuffix(src.Container); ctrSuffix != "" {
			for _, k := range rel.ProbeContentKeys() {
				add(k + "|ctr:" + ctrSuffix)
			}
			return nil, keys
		}
		for _, k := range rel.ProbeContentKeys() {
			add(k)
		}
		return &rel, keys

	case kindstream.PatternContainer:
		if src.Container == nil {
			return nil, keys
		}
		var b strings.Builder
		fmt.Fprintf(&b, "ctr|f=%s|m=%s|",
			strings.ToLower(strings.TrimSpace(src.Container.Format)),
			strings.ToLower(strings.TrimSpace(src.Container.Member)),
		)
		if src.Container.MemberIndex != nil {
			fmt.Fprintf(&b, "mi=%d|", *src.Container.MemberIndex)
		}
		for _, v := range src.Container.Volumes {
			fmt.Fprintf(&b, "v=%s:%s:%d|", strings.TrimSpace(v.URL), strings.TrimSpace(v.BlobRef), v.Size)
		}
		sum := sha256.Sum256([]byte(b.String()))
		add("kctr:" + hex.EncodeToString(sum[:12]))
		return nil, keys

	case kindstream.PatternFile:
		if p := strings.TrimSpace(src.Path); p != "" {
			sum := sha256.Sum256([]byte(pluginID + "|file|" + p))
			add("kfile:" + hex.EncodeToString(sum[:12]))
		}
		return nil, keys

	default:
		if u := strings.TrimSpace(src.URL); u != "" {
			sum := sha256.Sum256([]byte("url|" + u))
			add("ksrc:" + hex.EncodeToString(sum[:12]))
		}
		return nil, keys
	}
}

func (r *KindResolver) cachedSourceProbe(pluginID string, src core.PlayableSource, defaultMIME string, actualSize int64) (core.MediaInfo, bool) {
	if r == nil || r.probeCache == nil {
		return core.MediaInfo{}, false
	}
	rel, keys := sourceProbeIdentity(pluginID, src, defaultMIME)
	var (
		info core.MediaInfo
		hit  bool
	)
	if rel != nil {
		info, hit = r.probeCache.CachedReleaseProbe(*rel)
	}
	if !hit && len(keys) > 0 {
		info, hit = r.probeCache.CachedProbeByKeys(keys)
	}
	if !hit {
		return core.MediaInfo{}, false
	}
	// Refuse a cached entry whose size disagrees with the opened file or an
	// exact declared size (protects against replaced URLs or edited files).
	if actualSize > 0 && info.Size > 0 && info.Size != actualSize {
		return core.MediaInfo{}, false
	}
	if actualSize <= 0 && src.Size > 0 && !src.SizeApproximate && info.Size > 0 && info.Size != src.Size {
		return core.MediaInfo{}, false
	}
	return info, true
}

func (r *KindResolver) recordSourceProbe(ctx context.Context, pluginID string, src core.PlayableSource, defaultMIME string, info core.MediaInfo) {
	if r == nil || r.probeCache == nil {
		return
	}
	rel, keys := sourceProbeIdentity(pluginID, src, defaultMIME)
	if rel != nil {
		r.probeCache.RecordReleaseProbe(ctx, *rel, nil, info)
	}
	if len(keys) > 0 {
		r.probeCache.RecordProbeByKeys(ctx, keys, info)
	}
}

func (r *KindResolver) probeOpenByteSource(ctx context.Context, pluginID string, src core.PlayableSource, defaultMIME string, bs kindstream.ByteSource) (*core.MediaInfo, error) {
	if info, hit := r.cachedSourceProbe(pluginID, src, defaultMIME, bs.Size()); hit {
		return &info, nil
	}
	info, err := kindstream.ProbeByteSource(ctx, bs, r.probeStream)
	if err != nil {
		return nil, err
	}
	if info != nil {
		r.recordSourceProbe(ctx, pluginID, src, defaultMIME, *info)
	}
	return info, nil
}

func mediaProbeResultFromInfo(info core.MediaInfo, fallbackSize int64) core.MediaProbeResult {
	size := info.Size
	if size <= 0 {
		size = fallbackSize
	}
	res := core.MediaProbeResult{
		DurationSec: info.Duration.Seconds(),
		Container:   info.Container,
		SizeBytes:   size,
	}
	if v := info.Video(); v != nil {
		res.Width = v.Width
		res.Height = v.Height
	}
	return res
}

type readSeekByteSource struct {
	mu   sync.Mutex
	rc   io.ReadSeekCloser
	size int64
	name string
	mime string
}

func (s *readSeekByteSource) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.rc.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(s.rc, p)
}

func (s *readSeekByteSource) Size() int64  { return s.size }
func (s *readSeekByteSource) Name() string { return s.name }
func (s *readSeekByteSource) MIME() string { return s.mime }
func (s *readSeekByteSource) Close() error { return s.rc.Close() }

// ProbeMedia implements the plugin host's media.probe(source) backend (#712).
// It enforces per-pattern authorization (network allowlist, folder grants,
// plugin-owned blobs, secrets), reuses the durable release_probes cache when
// available, bounds every probe at plugins.MediaProbeTimeout (10 s), and
// opens release sources strictly cached-only without queuing downloads.
func (r *KindResolver) ProbeMedia(ctx context.Context, pluginID string, src core.PlayableSource) (core.MediaProbeResult, error) {
	pctx, cancel := context.WithTimeout(ctx, plugins.MediaProbeTimeout)
	defer cancel()
	pctx = core.WithIndexerPurpose(pctx, core.IndexerPurposeAvailability)

	var allowHost func(string) bool
	var secretFn func(context.Context, string) (string, error)
	if r.mgr != nil {
		allowHost = func(host string) bool {
			hctx, hcancel := context.WithTimeout(pctx, 5*time.Second)
			defer hcancel()
			return r.mgr.AllowNetworkHost(hctx, pluginID, host)
		}
		host := r.mgr.HostFor(pluginID)
		secretFn = func(sctx context.Context, name string) (string, error) {
			return host.SecretGet(sctx, name)
		}
	}

	pattern := inferSourcePattern(src)

	// Enforce pattern-specific access control BEFORE consulting the shared
	// release_probes cache so a plugin cannot probe a URL, folder, or blob it
	// is not granted access to.
	if blob := strings.TrimSpace(src.BlobRef); blob != "" && pattern != kindstream.PatternContainer && pattern != kindstream.PatternFile {
		if r.mgr == nil {
			return core.MediaProbeResult{}, plugins.ErrUnavailable
		}
		rc, err := r.mgr.HostFor(pluginID).CacheOpen(pctx, blob)
		if err != nil {
			return core.MediaProbeResult{}, err
		}
		defer func() { _ = rc.Close() }()
		var bs kindstream.ByteSource
		if existing, ok := rc.(kindstream.ByteSource); ok {
			bs = existing
		} else {
			sz, serr := rc.Seek(0, io.SeekEnd)
			if serr != nil {
				return core.MediaProbeResult{}, serr
			}
			if _, serr := rc.Seek(0, io.SeekStart); serr != nil {
				return core.MediaProbeResult{}, serr
			}
			bs = &readSeekByteSource{rc: rc, size: sz, name: src.ID, mime: src.MIME}
		}
		info, err := r.probeOpenByteSource(pctx, pluginID, src, "", bs)
		if err != nil {
			return core.MediaProbeResult{}, err
		}
		return mediaProbeResultFromInfo(*info, bs.Size()), nil
	}

	switch pattern {
	case kindstream.PatternFile:
		s := &StreamSources{mgr: r.mgr}
		resolved, _, err := s.validateLocalFile(pctx, pluginID, src.Path)
		if err != nil {
			return core.MediaProbeResult{}, err
		}
		f, err := s.openGrantedLocalFile(pctx, pluginID, resolved)
		if err != nil {
			return core.MediaProbeResult{}, err
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return core.MediaProbeResult{}, err
		}
		if !st.Mode().IsRegular() {
			return core.MediaProbeResult{}, core.ErrSourceUnavailable
		}
		bs := &kindLocalFile{File: f, size: st.Size(), mime: src.MIME}
		info, err := r.probeOpenByteSource(pctx, pluginID, src, "", bs)
		if err != nil {
			return core.MediaProbeResult{}, err
		}
		return mediaProbeResultFromInfo(*info, st.Size()), nil

	case kindstream.PatternRange, kindstream.PatternDownload, kindstream.PatternDirect:
		if _, err := kindstream.ParseURLTemplate(src.URL, allowHost); err != nil {
			if errors.Is(err, kindstream.ErrHostNotAllowed) {
				return core.MediaProbeResult{}, fmt.Errorf("%w: %v", plugins.ErrPermissionDenied, err)
			}
			return core.MediaProbeResult{}, err
		}
		if info, hit := r.cachedSourceProbe(pluginID, src, "", 0); hit {
			return mediaProbeResultFromInfo(info, info.Size), nil
		}

	case kindstream.PatternContainer:
		if src.Container == nil {
			return core.MediaProbeResult{}, &plugins.ValidationError{Fields: []plugins.FieldError{{Field: "container", Message: "container source requires container"}}}
		}
		for _, v := range src.Container.Volumes {
			if strings.TrimSpace(v.URL) != "" {
				if _, err := kindstream.ParseURLTemplate(v.URL, allowHost); err != nil {
					if errors.Is(err, kindstream.ErrHostNotAllowed) {
						return core.MediaProbeResult{}, fmt.Errorf("%w: %v", plugins.ErrPermissionDenied, err)
					}
					return core.MediaProbeResult{}, err
				}
			}
			if blob := strings.TrimSpace(v.BlobRef); blob != "" {
				if r.mgr == nil {
					return core.MediaProbeResult{}, plugins.ErrUnavailable
				}
				rc, err := r.mgr.HostFor(pluginID).CacheOpen(pctx, blob)
				if err != nil {
					return core.MediaProbeResult{}, err
				}
				_ = rc.Close()
			}
		}
		if info, hit := r.cachedSourceProbe(pluginID, src, "", 0); hit {
			return mediaProbeResultFromInfo(info, info.Size), nil
		}

	case kindstream.PatternRelease:
		if src.Release == nil {
			return core.MediaProbeResult{}, &plugins.ValidationError{Fields: []plugins.FieldError{{Field: "release", Message: "release source requires release"}}}
		}
		if info, hit := r.cachedSourceProbe(pluginID, src, "", 0); hit {
			return mediaProbeResultFromInfo(info, info.Size), nil
		}
	}

	// Cache miss: open the source (strictly cached-only for releases, with
	// duration verification disabled so we report the observed metadata).
	openSrc := src
	openSrc.DurationToleranceSec = 0
	if openSrc.Release != nil {
		relCopy := *openSrc.Release
		relCopy.CachedOnly = true
		openSrc.Release = &relCopy
	}
	maxDownload := int64(mediaProbeMaxDownloadBytes)
	if openSrc.MaxBytes > 0 && openSrc.MaxBytes < maxDownload {
		maxDownload = openSrc.MaxBytes
	}

	probeDeps := kindstream.Deps{
		LimitRangeTransport: r.mgr.LimitRangeTransport,
		HTTP:                r.http,
		OpenRelease: func(octx context.Context, hint core.ReleaseHint) (core.MediaSource, error) {
			hint.CachedOnly = true
			return r.openReleaseWithCategory(octx, hint, 0)
		},
		OpenReleaseSource: func(octx context.Context, s core.PlayableSource) (core.MediaSource, error) {
			if s.Release != nil {
				rc := *s.Release
				rc.CachedOnly = true
				s.Release = &rc
			}
			return r.openReleaseForSourceWithCategory(octx, s, "", 0, secretFn)
		},
		Secret:           secretFn,
		AllowHost:        allowHost,
		MaxDownloadBytes: maxDownload,
		Backoff:          r.backoff,
	}
	if openSrc.Container != nil && r.blobs != nil {
		for _, v := range openSrc.Container.Volumes {
			if blob := strings.TrimSpace(v.BlobRef); blob != "" {
				if ns := r.blobs.NamespaceForPluginBlob(pluginID, blob); ns != nil {
					probeDeps.Cache = ns
					break
				}
			}
		}
	}

	bs, err := kindstream.Open(pctx, openSrc, probeDeps)
	if err != nil {
		if errors.Is(err, kindstream.ErrHostNotAllowed) {
			return core.MediaProbeResult{}, fmt.Errorf("%w: %v", plugins.ErrPermissionDenied, err)
		}
		return core.MediaProbeResult{}, err
	}
	defer func() { _ = bs.Close() }()

	info, err := r.probeOpenByteSource(pctx, pluginID, src, "", bs)
	if err != nil {
		if errors.Is(err, transcode.ErrProbeIncomplete) {
			return core.MediaProbeResult{}, fmt.Errorf("%w: could not read container metadata", plugins.ErrUnavailable)
		}
		return core.MediaProbeResult{}, err
	}
	return mediaProbeResultFromInfo(*info, bs.Size()), nil
}

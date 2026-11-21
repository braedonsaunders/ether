package sessions

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/transcode"
)

var _ frontend.SessionSubtitleArtifacts = (*Broker)(nil)

func (b *Broker) authorizeSubtitleArtifact(ctx context.Context, a frontend.SessionAccess, id string, index int) (*brokerSession, *core.PlaySession, core.StreamInfo, context.Context, func(), error) {
	if index < 0 {
		return nil, nil, core.StreamInfo{}, ctx, nil, core.ErrNotFound
	}
	_ = b.restoreNeutralBindingMode(ctx, id, &a, "", string(a.Protocol), true)
	s, viewer, err := b.access(ctx, a, id)
	if err != nil {
		return nil, nil, core.StreamInfo{}, ctx, nil, err
	}
	if viewer.ID != s.public.ViewerID {
		viewer, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
	}
	if err != nil || !b.currentMediaPolicy(ctx, viewer, s) {
		return nil, nil, core.StreamInfo{}, ctx, nil, core.ErrUnauthorized
	}
	if !b.neutralMediaCurrent(ctx, s) {
		return nil, nil, core.StreamInfo{}, ctx, nil, core.ErrNotAvailable
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		return nil, nil, core.StreamInfo{}, ctx, nil, core.ErrNotFound
	}
	if s.sourcePinned && sourceRevision(ps) != s.sourceETag {
		return nil, nil, core.StreamInfo{}, ctx, nil, frontend.ErrSourceGone
	}
	st, ok := subtitleStream(ps.Info, index)
	if !ok || transcode.IsImageSubtitle(st.Codec) {
		return nil, nil, core.StreamInfo{}, ctx, nil, core.ErrNotFound
	}
	if st.External && (!strings.HasPrefix(st.ExternalURL, "ether-subtitle:") || b.deps.OpenSubtitle == nil) {
		return nil, nil, core.StreamInfo{}, ctx, nil, core.ErrNotAvailable
	}

	tracker := &core.ReadRetirement{}
	b.mu.Lock()
	live := b.sessions[id]
	if live == nil || live.playID != s.playID || !live.public.StartedAt.Equal(s.public.StartedAt) || live.sourceETag != s.sourceETag {
		b.mu.Unlock()
		return nil, nil, core.StreamInfo{}, ctx, nil, core.ErrNotFound
	}
	live.servingReads = append(live.servingReads, tracker)
	copied := *live
	s = &copied
	b.mu.Unlock()

	readCtx := core.WithReadRetirement(ctx, tracker)
	if remote, ok := b.deps.Playback.(interface{ Touch(string) }); ok && s.playID != "" {
		remote.Touch(s.playID)
	}
	var endMediaRead func()
	if !s.live && s.playID != "" {
		if reader, ok := b.deps.Playback.(interface{ BeginMediaRead(string) func() }); ok {
			endMediaRead = reader.BeginMediaRead(s.playID)
		}
	}
	release := func() {
		if endMediaRead != nil {
			endMediaRead()
		}
		<-tracker.Seal()
		b.mu.Lock()
		if cur := b.sessions[id]; cur != nil {
			kept := cur.servingReads[:0]
			for _, item := range cur.servingReads {
				if item != tracker {
					kept = append(kept, item)
				}
			}
			cur.servingReads = kept
		}
		b.mu.Unlock()
	}
	return s, ps, st, readCtx, release, nil
}

func (b *Broker) withSessionSource(ctx context.Context, s *brokerSession, ps *core.PlaySession, fn func(ctx context.Context, src io.ReaderAt, size int64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.playID == "" {
		return core.ErrNotAvailable
	}
	res, ok := b.deps.Playback.(core.PlaybackSourceResource)
	if !ok {
		return core.ErrNotAvailable
	}
	rev := sourceRevision(ps)
	if s.sourcePinned {
		rev = s.sourceETag
	}
	// This capability is authoritative. Neither a denial nor an unavailable
	// generation permits falling back to an unscoped reader or session URL.
	return res.WithSourceResource(ctx, s.playID, rev, fn)
}

func (b *Broker) readEmbeddedAuthoredASS(ctx context.Context, s *brokerSession, ps *core.PlaySession, st core.StreamInfo) ([]byte, error) {
	if st.External || !subtitles.IsASSCodec(st.Codec) {
		return nil, core.ErrNotAvailable
	}
	var rawASS []byte
	err := b.withSessionSource(ctx, s, ps, func(rctx context.Context, src io.ReaderAt, size int64) error {
		extracted, extractErr := subtitles.ReadMKVEmbeddedASS(rctx, src, size, st.Index)
		if extractErr != nil {
			return extractErr
		}
		rawASS = extracted
		return nil
	})
	if err == nil && len(rawASS) > 0 {
		return rawASS, nil
	}

	if err != nil {
		return nil, err
	}
	return nil, core.ErrNotAvailable
}

// HasAuthoredASSSubtitle reports whether the subtitle stream at index has a
// complete, non-truncated authored ASS/SSA script available.
func (b *Broker) HasAuthoredASSSubtitle(ctx context.Context, a frontend.SessionAccess, id string, index int) bool {
	s, ps, st, readCtx, release, err := b.authorizeSubtitleArtifact(ctx, a, id, index)
	if err != nil {
		return false
	}
	defer release()
	if st.External || !subtitles.IsASSCodec(st.Codec) {
		return false
	}
	raw, err := b.readEmbeddedAuthoredASS(readCtx, s, ps, st)
	return err == nil && len(raw) > 0
}

var _ frontend.SubtitleSidecarReadiness = (*Broker)(nil)

// SubtitleSidecarReady reports whether serving the track at index is a cache
// read or a progressive stream rather than an extraction that reads the whole
// source. It authorizes the same way SubtitleURL does, without registering a
// serving read, so it stays cheap enough to call while a plan is assembled.
func (b *Broker) SubtitleSidecarReady(ctx context.Context, a frontend.SessionAccess, id string, index int) bool {
	if index < 0 {
		return false
	}
	s, viewer, err := b.access(ctx, a, id)
	if err != nil {
		return false
	}
	if viewer.ID != s.public.ViewerID {
		viewer, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
	}
	if err != nil || !b.currentMediaPolicy(ctx, viewer, s) {
		return false
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		return false
	}
	st, ok := subtitleStream(ps.Info, index)
	if !ok || transcode.IsImageSubtitle(st.Codec) {
		return false
	}
	// An external track is fetched from the subtitle provider, and a track that
	// can be streamed progressively is teed as the source is read, so neither
	// pays for a walk over the whole file here.
	if st.External {
		return true
	}
	if b.subtitleCached(ctx, s, st) {
		return true
	}
	return b.progressiveSubtitle(s, ps.Info, st)
}

// subtitleCached reports whether the track's text is already extracted.
func (b *Broker) subtitleCached(ctx context.Context, s *brokerSession, st core.StreamInfo) bool {
	cache, ok := b.deps.Transcoder.(interface {
		CachedSubtitle(context.Context, int, string) ([]byte, bool)
	})
	if !ok {
		return false
	}
	_, cached := cache.CachedSubtitle(b.subtitleContext(ctx, s.playID), st.Index, st.Codec)
	return cached
}

// ServeASSSubtitle extracts or converts the subtitle stream at index as ASS/SSA.
// When requireAuthored is true, any missing or truncated authored ASS track
// fails closed with frontend.ErrSubtitleAuthoringUnavailable instead of
// falling back to simplified WebVTT conversion.
func (b *Broker) ServeASSSubtitle(ctx context.Context, a frontend.SessionAccess, id string, index int, syncOffsetMS int, requireAuthored bool) (frontend.SubtitleASSArtifact, error) {
	s, ps, st, readCtx, release, err := b.authorizeSubtitleArtifact(ctx, a, id, index)
	if err != nil {
		return frontend.SubtitleASSArtifact{}, err
	}
	defer release()

	if syncOffsetMS < -10000 || syncOffsetMS > 10000 {
		syncOffsetMS = 0
	}

	if !st.External && subtitles.IsASSCodec(st.Codec) {
		rawASS, assErr := b.readEmbeddedAuthoredASS(readCtx, s, ps, st)
		if assErr == nil && len(rawASS) > 0 {
			if syncOffsetMS != 0 {
				rawASS = subtitles.ShiftASS(rawASS, time.Duration(syncOffsetMS)*time.Millisecond)
			}
			return frontend.SubtitleASSArtifact{Data: rawASS, Authored: true}, nil
		}
		if errors.Is(assErr, core.ErrSourceChanged) {
			return frontend.SubtitleASSArtifact{}, frontend.ErrSourceGone
		}
		if errors.Is(assErr, context.Canceled) || errors.Is(assErr, context.DeadlineExceeded) || errors.Is(assErr, core.ErrUnauthorized) || errors.Is(assErr, core.ErrNotAvailable) {
			return frontend.SubtitleASSArtifact{}, assErr
		}
		if errors.Is(assErr, subtitles.ErrASSTrackIncomplete) {
			return frontend.SubtitleASSArtifact{}, frontend.ErrSubtitleAuthoringUnavailable
		}
		// Do not retry against a possibly replaced InputURL after a scoped read
		// fails. Authored tracks are served intact or report an error.
		return frontend.SubtitleASSArtifact{}, assErr
	} else if requireAuthored {
		return frontend.SubtitleASSArtifact{}, frontend.ErrSubtitleAuthoringUnavailable
	}

	rawText, err := b.extractSubtitleText(readCtx, s, st)
	if err != nil {
		return frontend.SubtitleASSArtifact{}, err
	}
	cues, err := subtitles.ParseCues(string(rawText))
	if err != nil && strings.Contains(strings.ToLower(string(rawText)), "[events]") {
		cues, err = subtitles.ParseASS(string(rawText))
	}
	if err != nil {
		return frontend.SubtitleASSArtifact{Data: []byte(subtitles.ASSHeader), Authored: false}, nil
	}
	if syncOffsetMS != 0 {
		if shifted, shiftErr := subtitles.Shift(cues, time.Duration(syncOffsetMS)*time.Millisecond, 1); shiftErr == nil {
			cues = shifted
		}
	}
	return frontend.SubtitleASSArtifact{Data: []byte(subtitles.RenderASS(cues)), Authored: false}, nil
}

func (b *Broker) extractSubtitleText(ctx context.Context, s *brokerSession, stream core.StreamInfo) ([]byte, error) {
	const maxSubtitle = 32 << 20
	if stream.External {
		if b.deps.OpenSubtitle == nil || !strings.HasPrefix(stream.ExternalURL, "ether-subtitle:") {
			return nil, core.ErrNotFound
		}
		reader, actual, err := b.openSubtitle(s, stream.ExternalURL, "vtt")
		if err != nil {
			return nil, core.ErrNotFound
		}
		defer reader.Close()
		raw, err := io.ReadAll(io.LimitReader(reader, maxSubtitle+1))
		if err != nil || len(raw) > maxSubtitle || actual != "vtt" {
			return nil, core.ErrNotAvailable
		}
		return raw, nil
	}
	if b.deps.ExtractSubtitle == nil {
		return nil, core.ErrNotFound
	}
	var buf limitedBuffer
	buf.limit = maxSubtitle
	if err := b.deps.ExtractSubtitle(b.subtitleContext(ctx, s.playID), b.deps.Playback.InputURL(s.playID), stream.Index, stream.Codec, &buf); err != nil {
		return nil, core.ErrNotAvailable
	}
	return buf.Bytes(), nil
}

func (b *Broker) readSessionMKVFonts(ctx context.Context, s *brokerSession, ps *core.PlaySession, st core.StreamInfo) ([]subtitles.FontAttachment, error) {
	if st.External || !subtitles.IsASSCodec(st.Codec) {
		return nil, core.ErrNotFound
	}
	var fonts []subtitles.FontAttachment
	err := b.withSessionSource(ctx, s, ps, func(rctx context.Context, src io.ReaderAt, size int64) error {
		var assScript []byte
		rawASS, assErr := subtitles.ReadMKVEmbeddedASS(rctx, src, size, st.Index)
		if assErr != nil {
			return assErr
		}
		assScript = rawASS
		extracted, fontErr := subtitles.ReadMKVFontAttachments(rctx, src, size, assScript)
		if fontErr != nil {
			return fontErr
		}
		fonts = extracted
		return nil
	})
	if err != nil {
		if errors.Is(err, subtitles.ErrFontPackExceedsLimit) || errors.Is(err, subtitles.ErrFontAttachmentSelectionUnavailable) {
			return nil, frontend.ErrSubtitleFontsUnavailable
		}
		if errors.Is(err, core.ErrSourceChanged) {
			return nil, frontend.ErrSourceGone
		}
		return nil, err
	}
	return fonts, nil
}

// SubtitleFontBundle returns the base64-encoded Matroska font attachment bundle
// for an embedded ASS/SSA subtitle stream.
func (b *Broker) SubtitleFontBundle(ctx context.Context, a frontend.SessionAccess, id string, index int) ([]frontend.SubtitleFontBundleItem, error) {
	s, ps, st, readCtx, release, err := b.authorizeSubtitleArtifact(ctx, a, id, index)
	if err != nil {
		return nil, err
	}
	defer release()
	fonts, err := b.readSessionMKVFonts(readCtx, s, ps, st)
	if err != nil {
		return nil, err
	}
	encoded := subtitles.EncodeFontBundle(fonts)
	items := make([]frontend.SubtitleFontBundleItem, 0, len(encoded))
	for _, item := range encoded {
		items = append(items, frontend.SubtitleFontBundleItem{
			Name: item.Name,
			Data: item.Data,
		})
	}
	return items, nil
}

// SubtitleFontAttachment returns a single binary Matroska font attachment by
// its attachment index for an embedded ASS/SSA subtitle stream.
func (b *Broker) SubtitleFontAttachment(ctx context.Context, a frontend.SessionAccess, id string, index int, fontIndex int) (frontend.SubtitleFontAttachment, error) {
	if fontIndex < 0 || fontIndex >= subtitles.MaxFontAttachments {
		return frontend.SubtitleFontAttachment{}, core.ErrNotFound
	}
	s, ps, st, readCtx, release, err := b.authorizeSubtitleArtifact(ctx, a, id, index)
	if err != nil {
		return frontend.SubtitleFontAttachment{}, err
	}
	defer release()
	fonts, err := b.readSessionMKVFonts(readCtx, s, ps, st)
	if err != nil {
		return frontend.SubtitleFontAttachment{}, err
	}
	for _, f := range fonts {
		if f.Index == fontIndex {
			return frontend.SubtitleFontAttachment{
				Index:    f.Index,
				Name:     f.Name,
				MIMEType: subtitles.FontAttachmentMIMEType(f.Name),
				Data:     f.Data,
			}, nil
		}
	}
	return frontend.SubtitleFontAttachment{}, core.ErrNotFound
}

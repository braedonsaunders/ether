package frontend

import (
	"context"
	"errors"
	"io"
	"strings"
)

// SubtitleExtractor extracts one embedded subtitle stream to a writer.
type SubtitleExtractor interface {
	ExtractSubtitle(ctx context.Context, inputURL string, streamIndex int, codec string, w io.Writer) error
}

// MaxSubtitleFontAttachments bounds the number of Matroska font attachments
// exposed per subtitle stream.
const MaxSubtitleFontAttachments = 32

var (
	// ErrSubtitleFontsUnavailable reports that embedded font attachments exceed
	// the retained limit and could not be narrowed to script-referenced fonts.
	ErrSubtitleFontsUnavailable = errors.New("subtitle font attachments unavailable")

	// ErrSubtitleAuthoringUnavailable reports that complete authored ASS/SSA
	// styling could not be extracted when authored fidelity is required.
	ErrSubtitleAuthoringUnavailable = errors.New("authored subtitle styling unavailable")
)

// SubtitleFontBundleItem is a base64-encoded font attachment returned in a
// subtitle font bundle response.
type SubtitleFontBundleItem struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

// SubtitleFontAttachment is a binary Matroska font attachment for an ASS/SSA
// subtitle stream.
type SubtitleFontAttachment struct {
	Index    int
	Name     string
	MIMEType string
	Data     []byte
}

// SubtitleASSArtifact contains rendered or extracted ASS/SSA bytes and reports
// whether full authored styling was preserved from the source container.
type SubtitleASSArtifact struct {
	Data     []byte
	Authored bool
}

// SessionSubtitleArtifacts is an optional host capability implemented by
// Sessions that can inspect and serve embedded ASS/SSA scripts and Matroska
// font attachments for an active playback session across both direct and HLS
// deliveries.
type SessionSubtitleArtifacts interface {
	HasAuthoredASSSubtitle(ctx context.Context, access SessionAccess, id string, index int) bool
	ServeASSSubtitle(ctx context.Context, access SessionAccess, id string, index int, syncMS int, requireAuthored bool) (SubtitleASSArtifact, error)
	SubtitleFontBundle(ctx context.Context, access SessionAccess, id string, index int) ([]SubtitleFontBundleItem, error)
	SubtitleFontAttachment(ctx context.Context, access SessionAccess, id string, index int, fontIndex int) (SubtitleFontAttachment, error)
}

// SubtitleSidecarReadiness reports whether a text subtitle track can be served
// as a sidecar without reading the whole source: either its text is already
// extracted, or this client can stream it progressively. It answers a question
// about cost, not about authorization, and must stay cheap enough to call while
// a playback plan is assembled.
//
// A track that is neither cached nor progressive is still deliverable — the
// extraction is just proportional to the length of the release. A frontend
// whose startup path blocks on that fetch can use this to omit the artifact and
// begin playback without the subtitle instead of stalling. Hosts that do not
// implement it keep the previous behaviour, and implementations are expected to
// default to false only when they can prove the read is cheap.
type SubtitleSidecarReadiness interface {
	SubtitleSidecarReady(ctx context.Context, access SessionAccess, id string, index int) bool
}

// IsASSSubtitleCodec reports whether a stream codec identifier names an ASS or
// SSA text subtitle track.
func IsASSSubtitleCodec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "ass", "ssa", "s_text/ass", "s_text/ssa", "text/x-ssa", "text/x-ass":
		return true
	default:
		return false
	}
}

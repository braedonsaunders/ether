package usenet

import (
	"context"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/sockerr"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

// Errors reported by the NNTP client and the fetcher. They are wrapped with
// context; test with errors.Is.
var (
	// ErrNoSuchArticle is a 430 (or 423) reply: this provider does not have
	// the article. The fetcher turns "every provider said 430" into
	// core.ErrArticleMissing.
	ErrNoSuchArticle = errors.New("nntp: no such article")
	// ErrAuthFailed means the provider rejected the credentials, excluding capacity replies.
	ErrAuthFailed = errors.New("nntp: authentication failed")
	// ErrAuthRequired is a 480 reply to a command.
	ErrAuthRequired = errors.New("nntp: authentication required")
	// ErrServiceUnavailable is a 400/502 greeting or reply: the server is
	// refusing service, usually because of too many connections.
	ErrServiceUnavailable = errors.New("nntp: service unavailable")
	// ErrConnectionLimit is a temporary account/session capacity refusal.
	ErrConnectionLimit = fmt.Errorf("nntp: connection limit: %w", ErrServiceUnavailable)
	// ErrNoSuchGroup is a 411 reply to GROUP.
	ErrNoSuchGroup = errors.New("nntp: no such group")
	// ErrCorrupt means an article was received but did not decode as valid
	// yEnc (bad CRC, wrong size, missing headers).
	ErrCorrupt = core.ErrArticleCorrupt
	// ErrInvalidMessageID rejects message-ids that would break the protocol.
	ErrInvalidMessageID = errors.New("usenet: invalid message-id")
	// ErrNoProviders means no provider is configured and enabled.
	ErrNoProviders = fmt.Errorf("usenet: no usable providers: %w", core.ErrSourceUnavailable)
	// ErrProviderBackoff means a provider is temporarily skipped after
	// repeated connection failures.
	ErrProviderBackoff = fmt.Errorf("usenet: provider backing off after connection failures: %w", core.ErrSourceUnavailable)
	// ErrProviderDisabled means a provider is disabled after an
	// authentication failure, until a background login probe succeeds or
	// its settings change.
	ErrProviderDisabled = fmt.Errorf("usenet: provider disabled after authentication failure: %w", core.ErrSourceUnavailable)
	// ErrPoolClosed means the provider was removed or the fetcher closed.
	ErrPoolClosed = errors.New("usenet: connection pool closed")
	// ErrConnBroken means a connection was used after an I/O failure.
	ErrConnBroken = errors.New("nntp: connection broken")
)

// errStaleReuse is a network failure on a socket the pool had already used.
// The server dropped it while it sat idle. It is not a provider fault: the
// command is retried once on a fresh dial and the breaker is left alone.
var errStaleReuse = errors.New("usenet: stale connection")

// ProtocolError is an unexpected NNTP status reply.
type ProtocolError struct {
	Cmd  string // command verb, e.g. "BODY"
	Code int
	Msg  string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("nntp: %s: unexpected reply %d %s", e.Cmd, e.Code, e.Msg)
}

// Unwrap maps well-known codes to sentinel errors.
func (e *ProtocolError) Unwrap() error {
	if (e.Code == 400 || e.Code == 481 || e.Code == 482 || e.Code == 502) && isConnLimitMsg(e.Msg) {
		return ErrConnectionLimit
	}
	switch e.Code {
	// 451 is how some providers (Giganews) refuse an article that was taken
	// down: unavailable here, so another provider may still have it.
	case 430, 423, 451:
		return ErrNoSuchArticle
	case 480:
		return ErrAuthRequired
	case 481, 482:
		return ErrAuthFailed
	case 400, 502:
		return ErrServiceUnavailable
	case 411:
		return ErrNoSuchGroup
	}
	return nil
}

// isConnLimitMsg spots providers that answer AUTHINFO with 481/482 when the
// account is merely over its connection limit, which must not be treated as
// bad credentials.
func isConnLimitMsg(msg string) bool {
	m := strings.ToLower(msg)
	for _, hint := range []string{"max", "simultaneous", "connection", "too many", "already logged in", "in use"} {
		if strings.Contains(m, hint) {
			return true
		}
	}
	return false
}

// isNetworkError reports I/O-level failures: the connection is unusable.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		sockerr.IsConnReset(err) || errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, ErrConnBroken) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// isTimeoutError reports a deadline or inactivity timeout: the connection
// it happened on is unusable, but it says nothing about its idle siblings.
func isTimeoutError(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ctxErr is ctx.Err(), but also reports DeadlineExceeded as soon as the
// deadline has passed: network deadlines derived from ctx can fire a moment
// before the context's own timer, and such failures are the caller's, not
// the provider's.
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
		return context.DeadlineExceeded
	}
	return nil
}

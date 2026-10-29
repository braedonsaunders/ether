package mediabrowser

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// unmatchedLogger never logs request URLs, query strings, headers or path
// segments: those can contain account credentials or scoped media grants.
// A bounded global rate limit avoids log amplification from untrusted clients.
type unmatchedLogger struct {
	mu         sync.Mutex
	last       time.Time
	suppressed uint64
}

func (l *unmatchedLogger) log(i *instance, req *http.Request, status int) {
	if i.host.Log == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	if !l.last.IsZero() && now.Sub(l.last) < time.Minute {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed
	l.suppressed = 0
	l.last = now
	l.mu.Unlock()
	sum := sha256.Sum256([]byte(req.URL.Path))
	i.host.Log.Info("unmatched MediaBrowser route", "method", safeHTTPMethod(req.Method), "status", status, "path_hash", hex.EncodeToString(sum[:8]), "suppressed", suppressed)
}

func safeHTTPMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch:
		return method
	default:
		return "OTHER"
	}
}

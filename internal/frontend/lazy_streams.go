package frontend

import (
	"context"
	"net/http"
)

// LazyStreams issues reusable, short-lived playback capabilities without opening
// a playback session. The issuing credential never appears in the returned URL.
type LazyStreams interface {
	Issue(context.Context, string, PlanRequest) (string, error)
	ServeHTTP(http.ResponseWriter, *http.Request)
	// ServeAuthenticated streams an authenticated GET using a stable broker session.
	ServeAuthenticated(http.ResponseWriter, *http.Request, string, PlanRequest)
}

// LANLazyStreams issues a capability only for the currently selected DLNA
// viewer and verified network peer. The host derives the viewer and device;
// callers cannot assert either identity. Every use revalidates both.
type LANLazyStreams interface {
	IssueLAN(context.Context, *http.Request, PlanRequest) (string, error)
	ServeHTTP(http.ResponseWriter, *http.Request)
}

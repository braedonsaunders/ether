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
}

// LANLazyStreams issues a capability only for the currently selected DLNA
// viewer and verified network peer. The host derives the viewer and device;
// callers cannot assert either identity. Every use revalidates both.
type LANLazyStreams interface {
	IssueLAN(context.Context, *http.Request, PlanRequest) (string, error)
	ServeHTTP(http.ResponseWriter, *http.Request)
}

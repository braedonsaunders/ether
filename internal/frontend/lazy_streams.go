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

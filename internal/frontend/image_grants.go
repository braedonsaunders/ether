package frontend

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
)

// ImageGrantRequest asks for one authorized image on one authenticated session.
// Credential is supplied in-process and must never be placed in an image URL.
type ImageGrantRequest struct {
	Credential string
	Device     core.Device
	Item       core.RatingKey
	Channel    string
	Image      Image
}

// ImageGrants issues and validates short-lived opaque image capabilities.
// Serve requires a capability, even when the client cannot send account tokens.
type ImageGrants interface {
	Issue(context.Context, ImageGrantRequest) (string, error)
	Revoke(context.Context, string, string) error
	Serve(http.ResponseWriter, *http.Request, string)
}

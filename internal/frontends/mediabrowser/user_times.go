package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// Times are retrieved with the authenticated surface credential, never an
// asserted owner identity. Unknown values and unavailable services are omitted.
func (i *instance) userDTOWithTimes(r *http.Request, v *frontend.Viewer, credential string) map[string]any {
	user := i.userDTO(r, v)
	if credential == "" || i.host.Identity == nil {
		return user
	}
	times, err := i.host.Identity.ViewerTimes(i.identityContext(r), credential, v.ID)
	if err != nil {
		return user
	}
	if !times.LastLogin.IsZero() {
		user["LastLoginDate"] = wireTime(times.LastLogin)
	}
	if !times.LastActivity.IsZero() {
		user["LastActivityDate"] = wireTime(times.LastActivity)
	}
	return user
}

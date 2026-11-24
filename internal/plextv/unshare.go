package plextv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrShareNotFound reports that the server does not share with the user.
var ErrShareNotFound = errors.New("plextv: no share for that user")

// UnshareServer removes this server's share with a user (by Plex user id,
// else username or email). Moving a viewer to another Plex server frees
// their seat here.
func (c *Client) UnshareServer(ctx context.Context, ownerToken string, userID int64, usernameOrEmail string) error {
	if c.id.ClientIdentifier == "" {
		return errors.New("plextv: client identifier required")
	}
	shares, err := c.listRawShares(ctx, ownerToken)
	if err != nil {
		return err
	}
	usernameOrEmail = strings.TrimSpace(usernameOrEmail)
	var id int64
	for _, s := range shares {
		if userID > 0 && s.userID == userID {
			id = s.id
			break
		}
		if userID <= 0 && usernameOrEmail != "" && (strings.EqualFold(s.username, usernameOrEmail) || strings.EqualFold(s.email, usernameOrEmail)) {
			id = s.id
			break
		}
	}
	if id == 0 {
		return ErrShareNotFound
	}
	_, err = c.do(ctx, request{
		method: http.MethodDelete,
		path:   "/api/servers/" + url.PathEscape(c.id.ClientIdentifier) + "/shared_servers/" + strconv.FormatInt(id, 10),
		token:  ownerToken,
		accept: "application/xml",
	})
	return err
}

// inviteLink finds an invite URL in a share reply: a string value that is
// an https URL naming an invite, or an invite token turned into the accept
// page. "" when the reply carries none.
func inviteLink(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	var found string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		if found != "" {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(k, e)
			}
		case []any:
			for _, e := range x {
				walk(key, e)
			}
		case string:
			s := strings.TrimSpace(x)
			lk := strings.ToLower(key)
			if strings.HasPrefix(s, "https://") && (strings.Contains(strings.ToLower(s), "invite") || strings.Contains(lk, "invite")) {
				if u, err := url.Parse(s); err == nil && (strings.HasSuffix(u.Hostname(), "plex.tv") || u.Hostname() == "plex.tv") {
					found = s
				}
			}
		}
	}
	walk("", v)
	return found
}

// AcceptInvitesURL is the Plex Web page where an invited viewer accepts
// pending library shares.
const AcceptInvitesURL = "https://app.plex.tv/desktop/#!/settings/manage-library-access"

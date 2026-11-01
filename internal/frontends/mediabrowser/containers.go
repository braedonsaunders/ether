package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// containerDTO describes only the collection or playlist returned by the
// policy-filtered Experience. Membership and paging remain owned by it.
func (i *instance) containerDTO(d frontend.ItemDetail, key frontend.RatingKey) (map[string]any, bool) {
	id := ids.RatingKeyGUID(key)
	if c := d.Collection; c != nil && c.Key == key {
		return map[string]any{"Id": id, "Name": c.Title, "Type": "BoxSet", "IsFolder": true, "ServerId": i.info.ID, "ChildCount": c.ChildCount, "Overview": c.Summary, "UserData": i.userDataDTO(id, d.UserData)}, true
	}
	if p := d.Playlist; p != nil && p.Key == key {
		return map[string]any{"Id": id, "Name": p.Title, "Type": "Playlist", "IsFolder": true, "ServerId": i.info.ID, "ChildCount": len(p.Items), "UserData": i.userDataDTO(id, d.UserData)}, true
	}
	return nil, false
}

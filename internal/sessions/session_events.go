package sessions

import "github.com/braedonsaunders/ether/internal/frontend"

// sessionEvent provides lifecycle metadata without sharing a grant or device
// credential with socket subscribers or cluster event consumers.
func sessionEvent(kind string, session frontend.Session, timeline *frontend.Timeline) frontend.Event {
	session.MediaPath = ""
	session.Device.Client = ""
	return frontend.Event{Type: kind, ViewerID: session.ViewerID, Item: session.Item, SessionID: session.ID, Session: &session, Timeline: timeline}
}

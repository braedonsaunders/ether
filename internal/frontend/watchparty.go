package frontend

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Watch-party failures are safe to show a client. They do not include the title
// a viewer is not allowed to see.
var (
	ErrPartyNotFound  = errors.New("watch party not found")
	ErrPartyForbidden = errors.New("watch party forbidden")
	ErrPartyDenied    = errors.New("watch party denied")
	ErrPartyConflict  = errors.New("watch party conflict")
)

// PartyActor is one device of one viewer on one protocol. The same viewer on
// Silo and Jellyfin is two actors, so both can sit in one party.
type PartyActor struct {
	Viewer   *core.Viewer
	Device   core.Device
	Protocol Protocol
}

// PartyCreate opens a room. RoomID may be empty; the engine mints a UUID.
// Mode is host_pick. Vote is refused until suggestion voting exists.
type PartyCreate struct {
	RoomID, Mode, Name string
}

// PartyJoin accepts a Silo code, a Silo invite token, or a Jellyfin group id.
type PartyJoin struct {
	Code, Token, GroupID string
}

// PartyPick is the title the room will play. File and library are Silo hints.
// Each member still opens their own broker session for that title.
type PartyPick struct {
	Item              core.RatingKey
	FileID, LibraryID string
}

// PartyInput is a play, pause, or seek. Rate 0 leaves the room rate unchanged.
type PartyInput struct {
	Action   string
	Position time.Duration
	Rate     float64
}

// PartyReport is a realtime membership update from either dialect.
type PartyReport struct {
	Kind                           string
	SessionID, CommandID, Playlist string
	Position                       time.Duration
	Rate                           float64
	Paused, Ready, Lobby, Ignore   *bool
	ClientSent, When               time.Time
	Playing                        bool
}

// PartyQueue is a Jellyfin playlist edit. Replace also arms playback.
type PartyQueue struct {
	Kind            string
	Items           []core.RatingKey
	Index           int
	PlaylistItemID  string
	Position        time.Duration
	Repeat, Shuffle string
}

// PartyMember is one couch in the room, as seen by a particular actor.
type PartyMember struct {
	ViewerID                                       int64
	Name                                           string
	Protocol                                       Protocol
	DeviceID                                       string
	Host, Self, Connected, Ready, Buffering, Lobby bool
	IgnoreWait, Blocked                            bool
}

// PartyQueueItem is one title in the shared queue.
type PartyQueueItem struct {
	Item           core.RatingKey
	ContentID      string
	PlaylistItemID string
}

// PartyView is the room as one member is allowed to see it.
type PartyView struct {
	RoomID, Code, JoinToken, RoomToken, InvitePath, Name string
	Phase, Playback, SelectionMode, GuestPolicy, Role    string
	SelectionRevision, Generation                        int64
	Item                                                 core.RatingKey
	ContentID, FileID, LibraryID                         string
	Paused                                               bool
	Position                                             time.Duration
	AnchorAt                                             time.Time
	Rate                                                 float64
	HostConnected                                        bool
	CanTransport, CanManage, IgnoreWait                  bool
	AttachedSessionID                                    string
	Members                                              []PartyMember
	Repeat, Shuffle                                      string
	Queue                                                []PartyQueueItem
	QueueIndex                                           int
	UpdatedAt                                            time.Time
}

// PartyCommand is a scheduled transport change. Position is the playhead at
// ExecuteAt. Clients wait until that server time, which absorbs their latency.
type PartyCommand struct {
	ID, Action, Jellyfin, PlaylistItemID, SessionID string
	Position                                        time.Duration
	ExecuteAt, IssuedAt                             time.Time
	Playback                                        string
	SelectionRevision                               int64
	Rate                                            float64
}

// PartyEvent is one message for one member's socket.
type PartyEvent struct {
	Type                 string
	View                 PartyView
	Command              PartyCommand
	GroupKind            string
	GroupData            any
	Reason               string
	ClientSent, Received time.Time
	Sent                 time.Time
	ErrorCode            string
	ErrorMessage         string
}

// PartyTicket is a single-use Silo socket credential.
type PartyTicket struct {
	Ticket                string
	ExpiresIn, MaxSeconds int
	Protocol              string
}

// PartySocket is the live Silo room connection.
type PartySocket struct {
	Actor  PartyActor
	Events <-chan PartyEvent
	Close  func()
}

// WatchParty is the shared party engine. Silo and Jellyfin adapters translate
// their own protocols onto it. A nil host field means the feature is off.
type WatchParty interface {
	Create(context.Context, PartyActor, PartyCreate) (PartyView, error)
	Join(context.Context, PartyActor, PartyJoin) (PartyView, error)
	Get(context.Context, PartyActor, string, string) (PartyView, error)
	Select(context.Context, PartyActor, string, string, PartyPick) (PartyView, error)
	Start(context.Context, PartyActor, string, string) (PartyView, error)
	Stop(context.Context, PartyActor, string, string) (PartyView, error)
	Policy(context.Context, PartyActor, string, string, string) (PartyView, error)
	End(context.Context, PartyActor, string, string) error
	Leave(context.Context, PartyActor) error
	List(context.Context, PartyActor) []PartyView
	Transport(context.Context, PartyActor, string, string, PartyInput) error
	Report(context.Context, PartyActor, PartyReport) error
	Queue(context.Context, PartyActor, PartyQueue) error
	IssueTicket(context.Context, PartyActor, string, string) (PartyTicket, error)
	OpenSocket(context.Context, string, string) (PartySocket, error)
	Subscribe(PartyActor) (<-chan PartyEvent, func())
}

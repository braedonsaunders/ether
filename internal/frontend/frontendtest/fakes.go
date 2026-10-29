// Package frontendtest supplies configurable fakes for protocol implementations.
package frontendtest

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Experience implements frontend.Experience with optional function hooks.
type Experience struct {
	ResolveFacetFunc         func(context.Context, *core.Viewer, core.Device, string, string, string) (core.FacetValue, error)
	SetLikesFunc             func(context.Context, *core.Viewer, core.RatingKey, *bool) error
	SetEntityFavoriteFunc    func(context.Context, *core.Viewer, frontend.FavoriteRef, bool) error
	EntityFavoritesFunc      func(context.Context, *core.Viewer, []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error)
	SimilarFunc              func(context.Context, *core.Viewer, core.Device, core.RatingKey, frontend.ItemQuery) (frontend.SearchPage, error)
	AncestorsFunc            func(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]core.Title, error)
	FacetsFunc               func(context.Context, *core.Viewer, core.Device, string, string) ([]core.FacetValue, error)
	LibraryItemsFunc         func(context.Context, *core.Viewer, core.Device, string, frontend.ItemQuery) (core.Page, error)
	SetPlayedFunc            func(context.Context, *core.Viewer, core.RatingKey) error
	SetUnplayedFunc          func(context.Context, *core.Viewer, core.RatingKey) error
	SetWatchlistFunc         func(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetFavoriteFunc          func(context.Context, *core.Viewer, core.RatingKey, bool) error
	SetRatingFunc            func(context.Context, *core.Viewer, core.RatingKey, float64) error
	RemoveFromContinueFunc   func(context.Context, *core.Viewer, core.RatingKey) error
	ReportRowInteractionFunc func(context.Context, frontend.RowInteraction) error
	ViewsFunc                func(context.Context, *core.Viewer) ([]frontend.View, error)
	RowsFunc                 func(context.Context, frontend.RowRequest) ([]frontend.Row, error)
	ItemFunc                 func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error)
	ChildrenFunc             func(context.Context, *core.Viewer, core.Device, core.RatingKey, frontend.ItemQuery) (core.Page, error)
	SearchFunc               func(context.Context, *core.Viewer, core.Device, string, frontend.ItemQuery) (frontend.SearchPage, error)
	UserDataFunc             func(context.Context, *core.Viewer, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
}

func (f Experience) Views(c context.Context, v *core.Viewer) ([]frontend.View, error) {
	if f.ViewsFunc != nil {
		return f.ViewsFunc(c, v)
	}
	return nil, nil
}
func (f Experience) Rows(c context.Context, r frontend.RowRequest) ([]frontend.Row, error) {
	if f.RowsFunc != nil {
		return f.RowsFunc(c, r)
	}
	return nil, nil
}
func (f Experience) Item(c context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
	if f.ItemFunc != nil {
		return f.ItemFunc(c, r)
	}
	return frontend.ItemDetail{}, nil
}
func (f Experience) Children(c context.Context, v *core.Viewer, d core.Device, k core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
	if f.ChildrenFunc != nil {
		return f.ChildrenFunc(c, v, d, k, q)
	}
	return core.Page{}, nil
}
func (f Experience) Similar(c context.Context, v *core.Viewer, d core.Device, k core.RatingKey, q frontend.ItemQuery) (frontend.SearchPage, error) {
	if f.SimilarFunc != nil {
		return f.SimilarFunc(c, v, d, k, q)
	}
	return frontend.SearchPage{}, nil
}
func (f Experience) Ancestors(c context.Context, v *core.Viewer, d core.Device, k core.RatingKey) ([]core.Title, error) {
	if f.AncestorsFunc != nil {
		return f.AncestorsFunc(c, v, d, k)
	}
	return nil, nil
}
func (f Experience) ResolveFacet(c context.Context, v *core.Viewer, d core.Device, view, kind, id string) (core.FacetValue, error) {
	if f.ResolveFacetFunc != nil {
		return f.ResolveFacetFunc(c, v, d, view, kind, id)
	}
	return core.FacetValue{}, nil
}
func (f Experience) Facets(c context.Context, v *core.Viewer, d core.Device, id, name string) ([]core.FacetValue, error) {
	if f.FacetsFunc != nil {
		return f.FacetsFunc(c, v, d, id, name)
	}
	return nil, nil
}
func (f Experience) LibraryItems(c context.Context, v *core.Viewer, d core.Device, id string, q frontend.ItemQuery) (core.Page, error) {
	if f.LibraryItemsFunc != nil {
		return f.LibraryItemsFunc(c, v, d, id, q)
	}
	return core.Page{}, nil
}
func (f Experience) Search(c context.Context, v *core.Viewer, d core.Device, s string, q frontend.ItemQuery) (frontend.SearchPage, error) {
	if f.SearchFunc != nil {
		return f.SearchFunc(c, v, d, s, q)
	}
	return frontend.SearchPage{}, nil
}
func (f Experience) UserData(c context.Context, v *core.Viewer, k []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	if f.UserDataFunc != nil {
		return f.UserDataFunc(c, v, k)
	}
	return nil, nil
}

func (f Experience) ReportRowInteraction(c context.Context, interaction frontend.RowInteraction) error {
	if f.ReportRowInteractionFunc != nil {
		return f.ReportRowInteractionFunc(c, interaction)
	}
	return nil
}

var _ frontend.RowInteractionReporter = Experience{}

func (f Experience) SetPlayed(c context.Context, v *core.Viewer, k core.RatingKey) error {
	if f.SetPlayedFunc != nil {
		return f.SetPlayedFunc(c, v, k)
	}
	return nil
}
func (f Experience) SetUnplayed(c context.Context, v *core.Viewer, k core.RatingKey) error {
	if f.SetUnplayedFunc != nil {
		return f.SetUnplayedFunc(c, v, k)
	}
	return nil
}
func (f Experience) SetFavorite(c context.Context, v *core.Viewer, k core.RatingKey, on bool) error {
	if f.SetFavoriteFunc != nil {
		return f.SetFavoriteFunc(c, v, k, on)
	}
	return nil
}
func (f Experience) SetEntityFavorite(c context.Context, v *core.Viewer, ref frontend.FavoriteRef, on bool) error {
	if f.SetEntityFavoriteFunc != nil {
		return f.SetEntityFavoriteFunc(c, v, ref, on)
	}
	return nil
}
func (f Experience) EntityFavorites(c context.Context, v *core.Viewer, refs []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
	if f.EntityFavoritesFunc != nil {
		return f.EntityFavoritesFunc(c, v, refs)
	}
	return nil, nil
}
func (f Experience) SetLikes(c context.Context, v *core.Viewer, k core.RatingKey, likes *bool) error {
	if f.SetLikesFunc != nil {
		return f.SetLikesFunc(c, v, k, likes)
	}
	return nil
}
func (f Experience) SetRating(c context.Context, v *core.Viewer, k core.RatingKey, rating float64) error {
	if f.SetRatingFunc != nil {
		return f.SetRatingFunc(c, v, k, rating)
	}
	return nil
}
func (f Experience) RemoveFromContinue(c context.Context, v *core.Viewer, k core.RatingKey) error {
	if f.RemoveFromContinueFunc != nil {
		return f.RemoveFromContinueFunc(c, v, k)
	}
	return nil
}

// Sessions implements frontend.Sessions with optional function hooks.
type Sessions struct {
	PlanFunc               func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error)
	OpenFunc               func(context.Context, frontend.OpenRequest) (frontend.Session, error)
	StartStreamFunc        func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error)
	SubtitleURLFunc        func(context.Context, frontend.SessionAccess, string, int, string) (string, error)
	ServeMediaFunc         func(http.ResponseWriter, *http.Request)
	ReportFunc             func(context.Context, frontend.SessionAccess, string, frontend.Timeline) error
	StopFunc               func(context.Context, frontend.SessionAccess, string) error
	CommandFunc            func(context.Context, *core.Viewer, string, frontend.Command) error
	UpdateCapabilitiesFunc func(context.Context, frontend.SessionAccess, frontend.SessionCapabilities) error
	CapabilitiesFunc       func(context.Context, frontend.SessionAccess) (frontend.SessionCapabilities, error)
	SessionsFunc           func(context.Context, frontend.SessionAccess) ([]frontend.Session, error)
	RecoverFunc            func(context.Context, string, string, time.Duration) error
	TranscodeForFunc       func(context.Context, string) (core.HLSSession, bool)
	EventsFunc             func(context.Context, frontend.EventScope) (<-chan frontend.Event, error)
}

func (f Sessions) Plan(c context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	if f.PlanFunc != nil {
		return f.PlanFunc(c, r)
	}
	return frontend.PlayPlan{}, nil
}
func (f Sessions) Open(c context.Context, r frontend.OpenRequest) (frontend.Session, error) {
	if f.OpenFunc != nil {
		return f.OpenFunc(c, r)
	}
	return frontend.Session{}, nil
}
func (f Sessions) StartStream(c context.Context, a frontend.SessionAccess, id string, r frontend.StreamRequest) (string, error) {
	if f.StartStreamFunc != nil {
		return f.StartStreamFunc(c, a, id, r)
	}
	return "", nil
}
func (f Sessions) SubtitleURL(c context.Context, a frontend.SessionAccess, id string, index int, format string) (string, error) {
	if f.SubtitleURLFunc != nil {
		return f.SubtitleURLFunc(c, a, id, index, format)
	}
	return "", core.ErrNotAvailable
}
func (f Sessions) ServeMedia(w http.ResponseWriter, r *http.Request) {
	if f.ServeMediaFunc != nil {
		f.ServeMediaFunc(w, r)
		return
	}
	http.NotFound(w, r)
}
func (f Sessions) Report(c context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	if f.ReportFunc != nil {
		return f.ReportFunc(c, a, id, t)
	}
	return nil
}
func (f Sessions) Stop(c context.Context, a frontend.SessionAccess, id string) error {
	if f.StopFunc != nil {
		return f.StopFunc(c, a, id)
	}
	return nil
}
func (f Sessions) Command(c context.Context, actor *core.Viewer, id string, cmd frontend.Command) error {
	if f.CommandFunc != nil {
		return f.CommandFunc(c, actor, id, cmd)
	}
	return nil
}
func (f Sessions) UpdateCapabilities(c context.Context, a frontend.SessionAccess, v frontend.SessionCapabilities) error {
	if f.UpdateCapabilitiesFunc != nil {
		return f.UpdateCapabilitiesFunc(c, a, v)
	}
	return nil
}
func (f Sessions) Capabilities(c context.Context, a frontend.SessionAccess) (frontend.SessionCapabilities, error) {
	if f.CapabilitiesFunc != nil {
		return f.CapabilitiesFunc(c, a)
	}
	return frontend.SessionCapabilities{}, nil
}
func (f Sessions) Sessions(c context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	if f.SessionsFunc != nil {
		return f.SessionsFunc(c, a)
	}
	return nil, nil
}
func (f Sessions) Recover(c context.Context, id, url string, at time.Duration) error {
	if f.RecoverFunc != nil {
		return f.RecoverFunc(c, id, url, at)
	}
	return nil
}
func (f Sessions) TranscodeFor(c context.Context, id string) (core.HLSSession, bool) {
	if f.TranscodeForFunc != nil {
		return f.TranscodeForFunc(c, id)
	}
	return nil, false
}
func (f Sessions) Events(c context.Context, s frontend.EventScope) (<-chan frontend.Event, error) {
	if f.EventsFunc != nil {
		return f.EventsFunc(c, s)
	}
	ch := make(chan frontend.Event)
	close(ch)
	return ch, nil
}

var _ frontend.Experience = Experience{}
var _ frontend.Sessions = Sessions{}

// Identity implements a configurable surface-bound identity for protocol tests.
type Identity struct {
	IssueRefreshFunc  func(context.Context, string, core.Device) (string, error)
	RefreshFunc       func(context.Context, string, core.Device) (string, string, error)
	ResolveFunc       func(context.Context, string) (*core.Viewer, error)
	PublicViewersFunc func(context.Context) ([]frontend.PublicViewer, error)
	SignInFunc        func(context.Context, string, string, core.Device) (string, error)
	InitiateFunc      func(context.Context, core.Device) (string, string, error)
	StatusFunc        func(context.Context, string) (bool, error)
	DetailsFunc       func(context.Context, string) (frontend.QuickConnectDetails, error)
	PollFunc          func(context.Context, string) (string, error)
	ApproveFunc       func(context.Context, *core.Viewer, string) error
	RevokeFunc        func(context.Context, string) error
	SetupCompleteFunc func() bool
}

func (f Identity) Resolve(ctx context.Context, credential string) (*core.Viewer, error) {
	if f.ResolveFunc != nil {
		return f.ResolveFunc(ctx, credential)
	}
	return nil, core.ErrUnauthorized
}
func (f Identity) PublicViewers(ctx context.Context) ([]frontend.PublicViewer, error) {
	if f.PublicViewersFunc != nil {
		return f.PublicViewersFunc(ctx)
	}
	return nil, nil
}
func (f Identity) SignIn(ctx context.Context, user, password string, d core.Device) (string, error) {
	if f.SignInFunc != nil {
		return f.SignInFunc(ctx, user, password, d)
	}
	return "", frontend.ErrIdentityUnavailable
}
func (f Identity) QuickConnectInitiate(ctx context.Context, d core.Device) (string, string, error) {
	if f.InitiateFunc != nil {
		return f.InitiateFunc(ctx, d)
	}
	return "", "", frontend.ErrIdentityUnavailable
}
func (f Identity) QuickConnectPoll(ctx context.Context, secret string) (string, error) {
	if f.PollFunc != nil {
		return f.PollFunc(ctx, secret)
	}
	return "", frontend.ErrIdentityUnavailable
}
func (f Identity) QuickConnectStatus(ctx context.Context, secret string) (bool, error) {
	if f.StatusFunc != nil {
		return f.StatusFunc(ctx, secret)
	}
	return false, frontend.ErrIdentityUnavailable
}
func (f Identity) QuickConnectDetails(ctx context.Context, secret string) (frontend.QuickConnectDetails, error) {
	if f.DetailsFunc != nil {
		return f.DetailsFunc(ctx, secret)
	}
	return frontend.QuickConnectDetails{}, frontend.ErrIdentityUnavailable
}
func (f Identity) QuickConnectApprove(ctx context.Context, viewer *core.Viewer, code string) error {
	if f.ApproveFunc != nil {
		return f.ApproveFunc(ctx, viewer, code)
	}
	return frontend.ErrIdentityUnavailable
}
func (f Identity) Revoke(ctx context.Context, grant string) error {
	if f.RevokeFunc != nil {
		return f.RevokeFunc(ctx, grant)
	}
	return frontend.ErrIdentityUnavailable
}
func (f Identity) SetupComplete() bool { return f.SetupCompleteFunc != nil && f.SetupCompleteFunc() }

var _ frontend.SurfaceIdentity = Identity{}

// Capabilities is a configurable fake for the host capability provider.
type Capabilities struct {
	BuildFunc func(context.Context, frontend.CapabilityRequest) (frontend.CapabilityResult, error)
}

func (f Capabilities) Build(ctx context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
	if f.BuildFunc != nil {
		return f.BuildFunc(ctx, req)
	}
	return frontend.CapabilityResult{}, nil
}

var _ frontend.Capabilities = Capabilities{}

// Preferences is a configurable fake for protocol presentation state.
type Preferences struct {
	GetFunc    func(context.Context, *core.Viewer, string, string) (json.RawMessage, error)
	PutFunc    func(context.Context, *core.Viewer, string, string, json.RawMessage) error
	DeleteFunc func(context.Context, *core.Viewer, string, string) error
}

func (f Preferences) Get(ctx context.Context, v *core.Viewer, client, key string) (json.RawMessage, error) {
	if f.GetFunc != nil {
		return f.GetFunc(ctx, v, client, key)
	}
	return nil, frontend.ErrPreferencesNotFound
}
func (f Preferences) Put(ctx context.Context, v *core.Viewer, client, key string, data json.RawMessage) error {
	if f.PutFunc != nil {
		return f.PutFunc(ctx, v, client, key, data)
	}
	return nil
}
func (f Preferences) Delete(ctx context.Context, v *core.Viewer, client, key string) error {
	if f.DeleteFunc != nil {
		return f.DeleteFunc(ctx, v, client, key)
	}
	return nil
}

var _ frontend.Preferences = Preferences{}

// LiveTV is a configurable fake for viewer-scoped live television.
type LiveTV struct {
	ChannelsFunc         func(context.Context, *core.Viewer) ([]frontend.LiveChannel, error)
	GuideFunc            func(context.Context, *core.Viewer, frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error)
	ResolveChannelFunc   func(context.Context, *core.Viewer, string) (frontend.LiveChannel, error)
	ResolveProgrammeFunc func(context.Context, *core.Viewer, string, frontend.LiveGuideQuery) (frontend.LiveProgramme, error)
}

func (f LiveTV) Channels(ctx context.Context, v *core.Viewer) ([]frontend.LiveChannel, error) {
	if f.ChannelsFunc != nil {
		return f.ChannelsFunc(ctx, v)
	}
	return nil, nil
}
func (f LiveTV) Guide(ctx context.Context, v *core.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
	if f.GuideFunc != nil {
		return f.GuideFunc(ctx, v, q)
	}
	return nil, nil
}
func (f LiveTV) ResolveChannelGUID(ctx context.Context, v *core.Viewer, id string) (frontend.LiveChannel, error) {
	if f.ResolveChannelFunc != nil {
		return f.ResolveChannelFunc(ctx, v, id)
	}
	return frontend.LiveChannel{}, core.ErrNotFound
}
func (f LiveTV) ResolveProgrammeGUID(ctx context.Context, v *core.Viewer, id string, q frontend.LiveGuideQuery) (frontend.LiveProgramme, error) {
	if f.ResolveProgrammeFunc != nil {
		return f.ResolveProgrammeFunc(ctx, v, id, q)
	}
	return frontend.LiveProgramme{}, core.ErrNotFound
}

var _ frontend.LiveTV = LiveTV{}

func (f Identity) IssueRefresh(ctx context.Context, token string, d core.Device) (string, error) {
	if f.IssueRefreshFunc != nil {
		return f.IssueRefreshFunc(ctx, token, d)
	}
	return "", frontend.ErrIdentityUnavailable
}
func (f Identity) Refresh(ctx context.Context, token string, d core.Device) (string, string, error) {
	if f.RefreshFunc != nil {
		return f.RefreshFunc(ctx, token, d)
	}
	return "", "", frontend.ErrIdentityUnavailable
}

func (f Experience) SetWatchlist(c context.Context, v *core.Viewer, k core.RatingKey, on bool) error {
	if f.SetWatchlistFunc != nil {
		return f.SetWatchlistFunc(c, v, k, on)
	}
	return nil
}

// TrackPreferences is a configurable fake for shared viewer track settings.
type TrackPreferences struct {
	GetFunc func(context.Context, *core.Viewer) (frontend.TrackPreferences, error)
	SetFunc func(context.Context, *core.Viewer, frontend.TrackPreferences) error
}

func (f TrackPreferences) Get(ctx context.Context, v *core.Viewer) (frontend.TrackPreferences, error) {
	if f.GetFunc != nil {
		return f.GetFunc(ctx, v)
	}
	return frontend.TrackPreferences{}, frontend.ErrPreferencesNotFound
}
func (f TrackPreferences) Set(ctx context.Context, v *core.Viewer, p frontend.TrackPreferences) error {
	if f.SetFunc != nil {
		return f.SetFunc(ctx, v, p)
	}
	return frontend.ErrPreferencesNotFound
}

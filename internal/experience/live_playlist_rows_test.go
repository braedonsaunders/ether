package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
)

type playlistRowsFake struct{ lists []core.Playlist }

func (f playlistRowsFake) List(_ context.Context, id int64) ([]core.Playlist, error) {
	if id != 1 {
		return nil, errors.New("wrong user")
	}
	return f.lists, nil
}
func TestPlaylistEntriesOwnershipAndOrder(t *testing.T) {
	now := time.Unix(100, 0)
	source := playlistRowsFake{[]core.Playlist{{UserID: 2, Title: "other", UpdatedAt: now.Add(time.Hour)}, {UserID: 1, Title: "old"}, {UserID: 1, Title: "new", UpdatedAt: now, Items: []core.RatingKey{99}}}}
	entries, more, err := PlaylistEntries(context.Background(), &core.User{ID: 1}, source, 1)
	if err != nil || !more || len(entries) != 1 || entries[0].Kind != frontend.RowEntryPlaylist || entries[0].Playlist.Title != "new" || len(entries[0].Playlist.Items) != 0 {
		t.Fatalf("entries=%+v more=%v err=%v", entries, more, err)
	}
	if _, _, err := PlaylistEntries(context.Background(), nil, source, 1); err == nil {
		t.Fatal("nil viewer accepted")
	}
}

type liveRowsFake struct {
	channels   []livetv.Channel
	programmes []livetv.Programme
}

func (f liveRowsFake) AllowedChannels(_ context.Context, u *core.User) ([]livetv.Channel, error) {
	if u.ID != 1 {
		return nil, errors.New("denied")
	}
	return f.channels, nil
}
func (f liveRowsFake) Guide(_ context.Context, u *core.User, _, _ time.Time) ([]livetv.Programme, error) {
	if u.ID != 1 {
		return nil, errors.New("denied")
	}
	return f.programmes, nil
}
func TestLiveEntriesUsesAllowedGuideAndOmitsSourceURLs(t *testing.T) {
	now := time.Unix(100, 0)
	f := liveRowsFake{channels: []livetv.Channel{{ID: "a", Name: "Allowed", GuideID: "guide", StreamURL: "secret"}}, programmes: []livetv.Programme{{ID: "future", ChannelID: "a", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Item: livetv.Item{Title: "Next"}}, {ID: "current", ChannelID: "a", Start: now.Add(-time.Hour), End: now.Add(time.Hour), Item: livetv.Item{Title: "Now"}}, {ID: "hidden", ChannelID: "b", Start: now, End: now.Add(time.Hour)}}}
	got, err := LiveEntries(context.Background(), &core.User{ID: 1}, f, now, now.Add(3*time.Hour), nil)
	if err != nil || len(got) != 1 || got[0].LiveChannel.GuideReference != "guide" || got[0].LiveChannel.Current.Title != "Now" || got[0].LiveChannel.Next.Title != "Next" || got[0].LiveChannel.Logo != "" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if _, err := LiveEntries(context.Background(), &core.User{ID: 2}, f, now, now.Add(time.Hour), nil); err == nil {
		t.Fatal("denied user accepted")
	}
}

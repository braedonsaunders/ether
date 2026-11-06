package plex

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/plex/model"
)

func TestPlaylistNotificationLifecycle(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	sub, _ := f.srv.notifier.subscribe([]string{"timeline"}, &ownerUser)
	defer f.srv.notifier.unsubscribe(sub)
	created := f.request(t, "POST", "/playlists?title=Live+list&type=video&uri=unresolved", map[string]string{"Accept": "application/json"})
	if created.Code != 200 {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var key string
	for _, want := range []struct {
		state           int
		metadata, title string
	}{{0, "created", ""}, {9, "deleted", "Live list"}} {
		if want.state == 9 {
			r := f.request(t, "DELETE", "/playlists/"+key, nil)
			if r.Code != http.StatusNoContent {
				t.Fatalf("delete: %d", r.Code)
			}
		}
		select {
		case msg := <-sub.ch:
			var root struct {
				NotificationContainer struct {
					TimelineEntry []struct {
						Identifier, SectionID, ItemID, Title, MetadataState string
						Type, State                                         int
					}
				}
			}
			if err := json.Unmarshal(msg.data, &root); err != nil {
				t.Fatal(err)
			}
			entries := root.NotificationContainer.TimelineEntry
			if len(entries) != 1 {
				t.Fatalf("entries: %s", msg.data)
			}
			e := entries[0]
			if e.Identifier != model.LibraryIdentifier || e.SectionID != "-1" || e.Type != 15 || e.State != want.state || e.MetadataState != want.metadata || e.Title != want.title {
				t.Fatalf("frame: %s", msg.data)
			}
			key = e.ItemID
			if len(msg.sse) != 1 || strings.Contains(string(msg.sse[0]), "NotificationContainer") {
				t.Fatalf("SSE: %s", msg.sse)
			}
		default:
			t.Fatal("missing timeline notification")
		}
	}
	select {
	case msg := <-sub.ch:
		t.Fatalf("extra notification: %s", msg.data)
	default:
	}
}

func TestRequestLogNotifications(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	sub, _ := f.srv.notifier.subscribe([]string{"log"}, &ownerUser)
	defer f.srv.notifier.unsubscribe(sub)
	f.request(t, "GET", "/identity?X-Plex-Token=never-log-this", nil)
	select {
	case msg := <-sub.ch:
		if msg.typ != "log" || !strings.Contains(string(msg.data), "LogSentNotification") || !strings.Contains(string(msg.data), "GET /identity [200]") || strings.Contains(string(msg.data), "never-log-this") {
			t.Fatalf("log: %s", msg.data)
		}
	default:
		t.Fatal("no live request log")
	}
}

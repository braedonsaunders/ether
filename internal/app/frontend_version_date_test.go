package app

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

func TestVersionDateUsesSourceOrPlexTitleRule(t *testing.T) {
	published := time.Unix(10, 0)
	updated := time.Unix(20, 0)
	released := time.Unix(30, 0)
	title := &core.Title{UpdatedAt: updated, Released: released}
	if !versionAddedAt(title, published).Equal(published) {
		t.Fatal("release date")
	}
	if !versionAddedAt(title, time.Time{}).Equal(updated) {
		t.Fatal("title update")
	}
	title.UpdatedAt = time.Time{}
	if !versionAddedAt(title, time.Time{}).Equal(released) {
		t.Fatal("release fallback")
	}
	title.Released = time.Time{}
	if !versionAddedAt(title, time.Time{}).IsZero() {
		t.Fatal("invented date")
	}
}

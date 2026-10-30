package experience

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RowInteractionPolicy rechecks persisted impressions against current scope
// before recording personalization events.
type RowInteractionPolicy struct {
	Libraries         []Library
	Visibility        Visibility
	Get               func(context.Context, core.RatingKey) (*core.Title, error)
	AllowEntry        func(context.Context, *core.User, string, string) bool
	CollectionMembers func(context.Context, *core.User, core.RatingKey, []core.NeutralEntryRef) bool
	Impression        func(*core.User, core.NeutralImpression)
	DetailOpen        func(context.Context, *core.User, *core.Title)
}

func (p RowInteractionPolicy) Allowed(ctx context.Context, u *core.User, subject ReceiptSubject) bool {
	if imp := subject.Impression; imp != nil {
		if len(imp.Entries) == 0 || imp.Kind == "" {
			return false
		}
		if imp.Collection != 0 && !p.AllowEntry(ctx, u, string(frontend.RowEntryCollection), imp.Collection.String()) {
			return false
		}
		if imp.Collection != 0 && !p.CollectionMembers(ctx, u, imp.Collection, imp.Entries) {
			return false
		}
		switch frontend.Placement(imp.Placement) {
		case frontend.PlacementHome:
		case frontend.PlacementLibrary:
			id, err := strconv.Atoi(imp.ViewID)
			valid := false
			for _, lib := range p.Libraries {
				if lib.ID == id {
					valid = true
					break
				}
			}
			if err != nil || !valid || !SectionAllowed(u, id) {
				return false
			}
		case frontend.PlacementDetail:
			if imp.Item == 0 {
				return false
			}
			t, err := p.Get(ctx, imp.Item)
			if err != nil || !p.Visibility.Visible(ctx, u, t) {
				return false
			}
		default:
			return false
		}
		for _, entry := range imp.Entries {
			if !p.AllowEntry(ctx, u, entry.Kind, entry.ID) {
				return false
			}
		}
		if subject.Kind == "row" {
			return true
		}
		i := strings.LastIndex(subject.ID, "/"+subject.Kind+"/")
		if i < 0 {
			return false
		}
		id := subject.ID[i+len(subject.Kind)+2:]
		for _, entry := range imp.Entries {
			if entry.Kind == subject.Kind && entry.ID == id {
				return true
			}
		}
		return false
	}
	return false
}

func (p RowInteractionPolicy) Record(ctx context.Context, u *core.User, subject ReceiptSubject, kind frontend.RowInteractionKind) error {
	if kind == frontend.RowImpression && subject.Impression != nil {
		p.Impression(u, *subject.Impression)
		return nil
	}
	if kind == frontend.RowClick && subject.Kind == string(frontend.RowEntryTitle) {
		idx := strings.LastIndex(subject.ID, "/title/")
		if idx < 0 {
			return ErrReceipt
		}
		key, err := core.ParseRatingKey(subject.ID[idx+len("/title/"):])
		if err != nil {
			return err
		}
		title, err := p.Get(ctx, key)
		if err != nil || !p.Visibility.Visible(ctx, u, title) {
			return ErrReceipt
		}
		p.DetailOpen(ctx, u, title)
		return nil
	}
	return errors.New("experience: unsupported row interaction")
}

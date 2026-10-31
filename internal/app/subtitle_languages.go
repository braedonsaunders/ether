package app

import (
	"context"
	"strings"
)

// syncSubtitleLanguages keeps auth_users.subtitle_languages aligned with the
// ordered list in trackprefs. Track selection and every protocol read the
// trackprefs list; the account column stays the same fact for callers that
// already load it from the user row.
func (a *App) syncSubtitleLanguages(ctx context.Context, user int64, languages []string) error {
	if user <= 0 || a.users == nil {
		return nil
	}
	current, err := a.users.ActiveUser(ctx, user)
	if err != nil {
		return err
	}
	if sameSubtitleLanguages(current.SubtitleLanguages, languages) {
		return nil
	}
	next := *current
	next.SubtitleLanguages = append([]string(nil), languages...)
	return a.users.UpdateUser(ctx, next)
}

func sameSubtitleLanguages(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(strings.TrimSpace(a[i]), strings.TrimSpace(b[i])) {
			return false
		}
	}
	return true
}

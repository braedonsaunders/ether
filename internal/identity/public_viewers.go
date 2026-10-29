package identity

import (
	"context"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// PublicViewers returns only active local sign-in identities, including
// Plex-linked viewers with a local password. The caller enforces picker opt-in.
func (l *Local) PublicViewers(ctx context.Context) ([]frontend.PublicViewer, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT u.id,MIN(l.subject) FROM auth_users u JOIN identity_links l ON l.viewer_id=u.id WHERE u.active=1 AND l.provider='local' AND l.password_hash<>'' GROUP BY u.id ORDER BY MIN(l.subject),u.id LIMIT 256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []frontend.PublicViewer{}
	for rows.Next() {
		var viewer frontend.PublicViewer
		if err := rows.Scan(&viewer.ID, &viewer.Name); err != nil {
			return nil, err
		}
		// This query only returns active viewers with a configured local password.
		configured := true
		viewer.HasConfiguredPassword = &configured
		result = append(result, viewer)
	}
	return result, rows.Err()
}

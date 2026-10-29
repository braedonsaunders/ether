package notinterested

import (
	"context"
	"database/sql"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("notinterested", 2, `
CREATE TABLE notinterested_revisions (viewer_id INTEGER PRIMARY KEY, revision INTEGER NOT NULL);
INSERT INTO notinterested_revisions(viewer_id,revision) SELECT viewer_id,1 FROM notinterested_titles GROUP BY viewer_id;
CREATE TRIGGER notinterested_insert_revision AFTER INSERT ON notinterested_titles BEGIN
 INSERT INTO notinterested_revisions(viewer_id,revision) VALUES(NEW.viewer_id,1) ON CONFLICT(viewer_id) DO UPDATE SET revision=notinterested_revisions.revision+1;
END;
CREATE TRIGGER notinterested_delete_revision AFTER DELETE ON notinterested_titles BEGIN
 INSERT INTO notinterested_revisions(viewer_id,revision) VALUES(OLD.viewer_id,1) ON CONFLICT(viewer_id) DO UPDATE SET revision=notinterested_revisions.revision+1;
END;`)
	store.RegisterDialect("notinterested", 2, store.Postgres, `
CREATE TABLE notinterested_revisions (viewer_id BIGINT PRIMARY KEY, revision BIGINT NOT NULL);
INSERT INTO notinterested_revisions(viewer_id,revision) SELECT viewer_id,1 FROM notinterested_titles GROUP BY viewer_id;
CREATE FUNCTION notinterested_bump_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE id BIGINT;
BEGIN
 IF TG_OP='INSERT' THEN id := NEW.viewer_id; ELSE id := OLD.viewer_id; END IF;
 INSERT INTO notinterested_revisions(viewer_id,revision) VALUES(id,1) ON CONFLICT(viewer_id) DO UPDATE SET revision=notinterested_revisions.revision+1;
 RETURN NULL;
END;
$$;
CREATE TRIGGER notinterested_insert_revision AFTER INSERT ON notinterested_titles FOR EACH ROW EXECUTE FUNCTION notinterested_bump_revision();
CREATE TRIGGER notinterested_delete_revision AFTER DELETE ON notinterested_titles FOR EACH ROW EXECUTE FUNCTION notinterested_bump_revision();`)
}

// Snapshot reads entries and their shared revision in one database statement.
// The revision changes even if the set returns to an earlier value. Visibility
// filtering is applied after the database snapshot, before results are returned.
func (s *Service) Snapshot(ctx context.Context, v *core.User) ([]Entry, uint64, error) {
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT COALESCE(r.revision,0),t.title_key,t.added_at FROM (SELECT ? AS viewer_id) v LEFT JOIN notinterested_revisions r ON r.viewer_id=v.viewer_id LEFT JOIN notinterested_titles t ON t.viewer_id=v.viewer_id ORDER BY t.added_at DESC,t.title_key ASC`, viewer.ID)
	if err != nil {
		return nil, 0, err
	}
	type candidate struct {
		key string
		at  int64
	}
	var candidates []candidate
	var revision int64
	for rows.Next() {
		var key sql.NullString
		var at sql.NullInt64
		if err = rows.Scan(&revision, &key, &at); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if key.Valid && at.Valid {
			candidates = append(candidates, candidate{key.String, at.Int64})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	entries := make([]Entry, 0, len(candidates))
	for _, c := range candidates {
		key, e := core.ParseRatingKey(c.key)
		if e == nil && valid(key) && s.o.Allowed(ctx, viewer, key) {
			entries = append(entries, Entry{Key: key, AddedAt: time.Unix(0, c.at).UTC()})
		}
	}
	return entries, uint64(revision), nil
}

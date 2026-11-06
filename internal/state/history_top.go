package state

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	ddl := `
CREATE TABLE state_history_events(history_id INTEGER NOT NULL,rating_key INTEGER NOT NULL,user_id INTEGER NOT NULL,viewed_at INTEGER NOT NULL,PRIMARY KEY(history_id,rating_key));
CREATE INDEX state_history_events_item ON state_history_events(rating_key,user_id,viewed_at DESC);
CREATE INDEX state_history_events_latest ON state_history_events(rating_key,viewed_at DESC);
CREATE INDEX state_history_events_time ON state_history_events((viewed_at/1000000000),rating_key,user_id);
CREATE TABLE state_lifetime_events(history_id INTEGER NOT NULL,rating_key INTEGER NOT NULL,user_id INTEGER NOT NULL,viewed_at INTEGER NOT NULL,PRIMARY KEY(history_id,rating_key));
CREATE INDEX state_lifetime_events_item ON state_lifetime_events(rating_key,(viewed_at/1000000000),user_id);
CREATE TABLE state_history_rank(rating_key INTEGER NOT NULL,user_id INTEGER NOT NULL,kind INTEGER NOT NULL,history_views INTEGER NOT NULL,lifetime_views INTEGER NOT NULL,last_history INTEGER NOT NULL,last_lifetime INTEGER NOT NULL,PRIMARY KEY(rating_key,user_id));
CREATE INDEX state_history_rank_order ON state_history_rank(user_id,kind,history_views DESC,last_history DESC,rating_key);
CREATE INDEX state_history_rank_users ON state_history_rank(rating_key,history_views,user_id);
INSERT INTO state_history_events SELECT id,rating_key,user_id,viewed_at FROM state_history;
INSERT INTO state_history_events SELECT id,3000000000000000+((rating_key%1000000000000000)/10000000)*10000+((rating_key%1000000000000000)/10000)%1000,user_id,viewed_at FROM state_history WHERE rating_key/1000000000000000=4;
INSERT INTO state_history_events SELECT id,2000000000000000+(rating_key%1000000000000000)/10000000,user_id,viewed_at FROM state_history WHERE rating_key/1000000000000000=4;
INSERT INTO state_history_rank SELECT rating_key,user_id,rating_key/1000000000000000,count(*),count(*),max(viewed_at),max(viewed_at) FROM state_history_events GROUP BY rating_key,user_id;
INSERT INTO state_history_rank SELECT rating_key,0,rating_key/1000000000000000,count(*),count(*),max(viewed_at),max(viewed_at) FROM state_history_events GROUP BY rating_key;
INSERT INTO state_lifetime_events SELECT * FROM state_history_events;
CREATE TRIGGER state_history_event_add AFTER INSERT ON state_history_events BEGIN
 INSERT INTO state_lifetime_events VALUES(new.history_id,new.rating_key,new.user_id,new.viewed_at);
 INSERT INTO state_history_rank VALUES(new.rating_key,new.user_id,new.rating_key/1000000000000000,1,1,new.viewed_at,new.viewed_at) ON CONFLICT(rating_key,user_id) DO UPDATE SET history_views=history_views+1,lifetime_views=lifetime_views+1,last_history=max(last_history,new.viewed_at),last_lifetime=max(last_lifetime,new.viewed_at);
 INSERT INTO state_history_rank VALUES(new.rating_key,0,new.rating_key/1000000000000000,1,1,new.viewed_at,new.viewed_at) ON CONFLICT(rating_key,user_id) DO UPDATE SET history_views=history_views+1,lifetime_views=lifetime_views+1,last_history=max(last_history,new.viewed_at),last_lifetime=max(last_lifetime,new.viewed_at);
END;
CREATE TRIGGER state_history_event_remove AFTER DELETE ON state_history_events BEGIN
 UPDATE state_history_rank SET history_views=max(0,history_views-1),last_history=coalesce((SELECT max(viewed_at) FROM state_history_events WHERE rating_key=old.rating_key AND user_id=old.user_id),0) WHERE rating_key=old.rating_key AND user_id=old.user_id;
 UPDATE state_history_rank SET history_views=max(0,history_views-1),last_history=coalesce((SELECT max(viewed_at) FROM state_history_events WHERE rating_key=old.rating_key),0) WHERE rating_key=old.rating_key AND user_id=0;
END;
CREATE TRIGGER state_history_rank_add AFTER INSERT ON state_history BEGIN
 INSERT INTO state_history_events VALUES(new.id,new.rating_key,new.user_id,new.viewed_at);
 INSERT INTO state_history_events SELECT new.id,3000000000000000+((new.rating_key%1000000000000000)/10000000)*10000+((new.rating_key%1000000000000000)/10000)%1000,new.user_id,new.viewed_at WHERE new.rating_key/1000000000000000=4;
 INSERT INTO state_history_events SELECT new.id,2000000000000000+(new.rating_key%1000000000000000)/10000000,new.user_id,new.viewed_at WHERE new.rating_key/1000000000000000=4;
END;
CREATE TRIGGER state_history_rank_remove AFTER DELETE ON state_history BEGIN DELETE FROM state_history_events WHERE history_id=old.id;END;
`
	store.Register("state", 9, ddl)
	baseDDL := store.TranslateDDL(strings.Split(ddl, "CREATE TRIGGER")[0])
	store.RegisterDialect("state", 9, store.Postgres, baseDDL+historyPostgresTriggers)
}

// History insertion triggers maintain lifetime and removable history counts atomically.
func (s *State) HistoryTop(ctx context.Context, q core.HistoryQuery, kind core.KeyKind) ([]core.HistoryCount, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	q.Limit = min(q.Limit, 500)
	var query string
	var args []any
	if q.Since == nil && q.Until == nil && q.At == nil {
		query = `SELECT rating_key,history_views,last_history FROM state_history_rank WHERE user_id=? AND kind=? AND history_views>0 ORDER BY history_views DESC,last_history DESC,rating_key LIMIT ?`
		args = []any{q.UserID, int(kind), q.Limit}
	} else {
		where := []string{"rating_key>=?", "rating_key<?"}
		const base int64 = 1000000000000000
		args = []any{int64(kind) * base, int64(kind+1) * base}
		if q.UserID != 0 {
			where = append(where, "user_id=?")
			args = append(args, q.UserID)
		}
		for _, f := range []struct {
			value    *int64
			operator string
		}{{q.Since, ">="}, {q.Until, "<="}, {q.At, "="}} {
			if f.value != nil {
				where = append(where, "viewed_at/1000000000"+f.operator+"?")
				args = append(args, *f.value)
			}
		}
		args = append(args, q.Limit)
		query = `SELECT rating_key,count(*),max(viewed_at) FROM state_history_events WHERE ` + strings.Join(where, " AND ") + ` GROUP BY rating_key ORDER BY count(*) DESC,max(viewed_at) DESC,rating_key LIMIT ?`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []core.HistoryCount
	for rows.Next() {
		var h core.HistoryCount
		if err := rows.Scan(&h.Key, &h.Count, &h.LastAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		userSQL := `SELECT user_id FROM state_history_rank WHERE rating_key=? AND user_id>0 AND history_views>0 ORDER BY user_id LIMIT 500`
		userArgs := []any{int64(out[i].Key)}
		if q.Since != nil || q.Until != nil || q.At != nil {
			w := []string{"rating_key=?"}
			for _, f := range []struct {
				value    *int64
				operator string
			}{{q.Since, ">="}, {q.Until, "<="}, {q.At, "="}} {
				if f.value != nil {
					w = append(w, "viewed_at/1000000000"+f.operator+"?")
					userArgs = append(userArgs, *f.value)
				}
			}
			userSQL = `SELECT DISTINCT user_id FROM state_history_events WHERE ` + strings.Join(w, " AND ") + ` ORDER BY user_id LIMIT 500`
		}
		users, err := s.db.QueryContext(ctx, userSQL, userArgs...)
		if err != nil {
			return nil, err
		}
		for users.Next() {
			var uid int64
			if err := users.Scan(&uid); err != nil {
				users.Close()
				return nil, err
			}
			if q.UserID == 0 || q.UserID == uid {
				out[i].Users = append(out[i].Users, uid)
			}
		}
		err = users.Err()
		users.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *State) LifetimeUsers(ctx context.Context, key core.RatingKey, q core.HistoryQuery) ([]core.HistoryCount, error) {
	where := []string{"rating_key=?", "user_id>0", "lifetime_views>0"}
	args := []any{int64(key)}
	if q.UserID != 0 {
		where = append(where, "user_id=?")
		args = append(args, q.UserID)
	}

	query := `SELECT user_id,lifetime_views,last_lifetime FROM state_history_rank WHERE ` + strings.Join(where, " AND ") + ` ORDER BY lifetime_views DESC,last_lifetime DESC,user_id LIMIT 500`
	if q.Since != nil || q.Until != nil || q.At != nil {
		where = []string{"rating_key=?"}
		args = []any{int64(key)}
		if q.UserID != 0 {
			where = append(where, "user_id=?")
			args = append(args, q.UserID)
		}
		for _, f := range []struct {
			value    *int64
			operator string
		}{{q.Since, ">="}, {q.Until, "<="}, {q.At, "="}} {
			if f.value != nil {
				where = append(where, "viewed_at/1000000000"+f.operator+"?")
				args = append(args, *f.value)
			}
		}
		query = `SELECT user_id,count(*),max(viewed_at) FROM state_lifetime_events WHERE ` + strings.Join(where, " AND ") + ` GROUP BY user_id ORDER BY count(*) DESC,max(viewed_at) DESC,user_id LIMIT 500`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.HistoryCount
	for rows.Next() {
		var h core.HistoryCount
		var uid int64
		if err := rows.Scan(&uid, &h.Count, &h.LastAt); err != nil {
			return nil, err
		}
		h.Key = key
		h.Users = []int64{uid}
		out = append(out, h)
	}
	return out, rows.Err()
}

const historyPostgresTriggers = `
CREATE FUNCTION state_history_event_rollup() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
 INSERT INTO state_lifetime_events VALUES(NEW.history_id,NEW.rating_key,NEW.user_id,NEW.viewed_at);
 INSERT INTO state_history_rank VALUES(NEW.rating_key,NEW.user_id,NEW.rating_key/1000000000000000,1,1,NEW.viewed_at,NEW.viewed_at) ON CONFLICT(rating_key,user_id) DO UPDATE SET history_views=state_history_rank.history_views+1,lifetime_views=state_history_rank.lifetime_views+1,last_history=greatest(state_history_rank.last_history,NEW.viewed_at),last_lifetime=greatest(state_history_rank.last_lifetime,NEW.viewed_at);
 INSERT INTO state_history_rank VALUES(NEW.rating_key,0,NEW.rating_key/1000000000000000,1,1,NEW.viewed_at,NEW.viewed_at) ON CONFLICT(rating_key,user_id) DO UPDATE SET history_views=state_history_rank.history_views+1,lifetime_views=state_history_rank.lifetime_views+1,last_history=greatest(state_history_rank.last_history,NEW.viewed_at),last_lifetime=greatest(state_history_rank.last_lifetime,NEW.viewed_at);
 RETURN NEW;
 ELSE
 UPDATE state_history_rank SET history_views=greatest(0,history_views-1),last_history=coalesce((SELECT max(viewed_at) FROM state_history_events WHERE rating_key=OLD.rating_key AND user_id=OLD.user_id),0) WHERE rating_key=OLD.rating_key AND user_id=OLD.user_id;
 UPDATE state_history_rank SET history_views=greatest(0,history_views-1),last_history=coalesce((SELECT max(viewed_at) FROM state_history_events WHERE rating_key=OLD.rating_key),0) WHERE rating_key=OLD.rating_key AND user_id=0;
 RETURN OLD;
 END IF;
END; $$;
CREATE TRIGGER state_history_event_add AFTER INSERT OR DELETE ON state_history_events FOR EACH ROW EXECUTE FUNCTION state_history_event_rollup();
CREATE FUNCTION state_history_project() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN DELETE FROM state_history_events WHERE history_id=OLD.id; RETURN OLD; END IF;
 INSERT INTO state_history_events VALUES(NEW.id,NEW.rating_key,NEW.user_id,NEW.viewed_at);
 IF NEW.rating_key/1000000000000000=4 THEN
 INSERT INTO state_history_events VALUES(NEW.id,3000000000000000+((NEW.rating_key%1000000000000000)/10000000)*10000+((NEW.rating_key%1000000000000000)/10000)%1000,NEW.user_id,NEW.viewed_at);
 INSERT INTO state_history_events VALUES(NEW.id,2000000000000000+(NEW.rating_key%1000000000000000)/10000000,NEW.user_id,NEW.viewed_at);
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER state_history_rank_add AFTER INSERT OR DELETE ON state_history FOR EACH ROW EXECUTE FUNCTION state_history_project();
`

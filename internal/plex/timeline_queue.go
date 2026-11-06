package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"strconv"
)

// timelineQueue resolves an explicit queue item before watch persistence. An
// omitted item selector follows the owning client's first matching queue row.
// Cluster fallback is capped at the same 50 recent queues as selectShared.
func (s *playQueueStore) timelineQueue(user int64, client string, key core.RatingKey, raw string, present bool) core.RatingKey {
	itemID, _ := strconv.ParseInt(raw, 10, 64)
	if present && itemID <= 0 {
		return key
	}
	find := func(q *playQueue) (int, bool) {
		if q.userID != user || q.clientID != client {
			return 0, false
		}
		for i, it := range q.items {
			if (present && it.id == itemID) || (!present && it.key == key) {
				return i, true
			}
		}
		return 0, false
	}
	var qid int64
	s.mu.Lock()
	for _, q := range s.queues {
		if i, ok := find(q); ok {
			key = q.items[i].key
			qid = q.id
			break
		}
	}
	s.mu.Unlock()
	if qid == 0 && s.cl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		rows, err := s.cl.db.QueryContext(ctx, `SELECT id,rev,data,updated_at FROM plex_playqueues WHERE user_id=? ORDER BY updated_at DESC LIMIT 50`, user)
		if err != nil {
			return key
		}
		for rows.Next() {
			var id, rev, updated int64
			var data string
			if rows.Scan(&id, &rev, &data, &updated) != nil {
				continue
			}
			q, err := decodeQueue(id, user, rev, updated, data)
			if err != nil {
				continue
			}
			if i, ok := find(q); ok {
				key = q.items[i].key
				qid = id
				s.cache(q)
				break
			}
		}
		rows.Close()
	}
	if qid != 0 {
		_, _ = s.with(qid, user, s.now(), func(q *playQueue) {
			if i, ok := find(q); ok {
				q.selected = i
			}
		})
	}
	return key
}

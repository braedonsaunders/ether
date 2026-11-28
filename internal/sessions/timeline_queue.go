package sessions

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"strconv"
)

// TimelineQueue resolves an explicit queue item before watch persistence. An
// omitted item selector follows the owning client's first matching queue row.
// Cluster fallback is capped at the same 50 recent queues as selectShared.
func (s *PlayQueues) TimelineQueue(user int64, client string, key core.RatingKey, raw string, present bool) core.RatingKey {
	itemID, _ := strconv.ParseInt(raw, 10, 64)
	if present && itemID <= 0 {
		return key
	}
	find := func(q *PlayQueue) (int, bool) {
		if q.UserID != user || q.ClientID != client {
			return 0, false
		}
		for i, it := range q.Items {
			// A trailer, pre-roll or other extra is never re-attributed to
			// the queue item a client names beside it: that is how a
			// detail-page trailer would become the feature's progress.
			if present && it.ID == itemID && key.Kind() == core.KindExtra && it.Key != key {
				continue
			}
			if (present && it.ID == itemID) || (!present && it.Key == key) {
				return i, true
			}
		}
		return 0, false
	}
	var qid int64
	now := s.now()
	s.mu.Lock()
	for _, q := range s.queues {
		if i, ok := find(q); ok {
			key = q.Items[i].Key
			// Keep active queues alive without rewriting them on every timeline.
			if q.Selected == i && now.Sub(q.UpdatedAt) <= queueCacheTTL {
				s.mu.Unlock()
				return key
			}
			qid = q.ID
			break
		}
	}
	s.mu.Unlock()
	if qid == 0 && s.cl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), playQueueDBTimeout)
		defer cancel()
		rows, err := s.cl.DB.QueryContext(ctx, `SELECT id,rev,data,updated_at FROM plex_playqueues WHERE user_id=? ORDER BY updated_at DESC LIMIT 50`, user)
		if err != nil {
			return key
		}
		for rows.Next() {
			var id, rev, updated int64
			var data string
			if rows.Scan(&id, &rev, &data, &updated) != nil {
				continue
			}
			q, err := DecodeQueue(id, user, rev, updated, data)
			if err != nil {
				continue
			}
			if i, ok := find(q); ok {
				key = q.Items[i].Key
				s.cache(q)
				if q.Selected != i || now.Sub(q.UpdatedAt) > queueCacheTTL {
					qid = id
				}
				break
			}
		}
		rows.Close()
	}
	if qid != 0 {
		_, _ = s.With(qid, user, now, func(q *PlayQueue) {
			if i, ok := find(q); ok {
				q.Selected = i
			}
		})
	}
	return key
}

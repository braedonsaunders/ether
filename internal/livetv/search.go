package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// FTS docids and readiness are disposable engine-local metadata. Keeping
	// readiness in a dump without its FTS tables would skip the next backfill.
	for _, table := range []string{"livetv_search_state", "livetv_search_channel_keys", "livetv_search_programme_keys"} {
		store.RegisterLocalTable(table)
	}
	store.RegisterRestore("livetv_search", func(ctx context.Context, tx *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		// Keep the migration seed for active: SQLite starts idle; PostgreSQL
		// maintains GIN indexes unconditionally and starts active.
		_, err := tx.ExecContext(ctx, `UPDATE livetv_search_state SET ready=0 WHERE id=1`)
		return nil, err
	}, "livetv_search_channel_keys", "livetv_search_programme_keys", "livetv_search_channels", "livetv_search_programmes")

	// Index changes happen in the import transaction, without a worker or polling.
	store.Register("livetv", 21, `
 CREATE TABLE livetv_search_state(id INTEGER PRIMARY KEY, active INTEGER NOT NULL, ready INTEGER NOT NULL); INSERT INTO livetv_search_state VALUES(1,0,0);
 CREATE TABLE livetv_search_channel_keys(docid INTEGER PRIMARY KEY,id TEXT NOT NULL UNIQUE);
 CREATE TABLE livetv_search_programme_keys(docid INTEGER PRIMARY KEY,source TEXT NOT NULL,channel TEXT NOT NULL,start BIGINT NOT NULL,UNIQUE(source,channel,start));
 CREATE VIRTUAL TABLE livetv_search_channels USING fts5(id UNINDEXED, body, tokenize='unicode61');
 CREATE TRIGGER livetv_search_channels_insert AFTER INSERT ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
 INSERT INTO livetv_search_channel_keys(id) VALUES(new.id) ON CONFLICT DO NOTHING;
 INSERT OR REPLACE INTO livetv_search_channels(rowid,id,body) VALUES((SELECT docid FROM livetv_search_channel_keys WHERE id=new.id),new.id,json_extract(new.data,'$.name') || ' ' || coalesce(json_extract(new.data,'$.number'),'') || ' ' || coalesce(json_extract(new.data,'$.guideId'),'')); END;
 CREATE TRIGGER livetv_search_channels_delete AFTER DELETE ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN DELETE FROM livetv_search_channels WHERE rowid=(SELECT docid FROM livetv_search_channel_keys WHERE id=old.id); DELETE FROM livetv_search_channel_keys WHERE id=old.id; END;
 CREATE TRIGGER livetv_search_channels_update AFTER UPDATE ON livetv_channels WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
 DELETE FROM livetv_search_channels WHERE rowid=(SELECT docid FROM livetv_search_channel_keys WHERE id=old.id); DELETE FROM livetv_search_channel_keys WHERE id=old.id; INSERT INTO livetv_search_channel_keys(id) VALUES(new.id) ON CONFLICT DO NOTHING;
 INSERT OR REPLACE INTO livetv_search_channels(rowid,id,body) VALUES((SELECT docid FROM livetv_search_channel_keys WHERE id=new.id),new.id,json_extract(new.data,'$.name') || ' ' || coalesce(json_extract(new.data,'$.number'),'') || ' ' || coalesce(json_extract(new.data,'$.guideId'),'')); END;
 CREATE VIRTUAL TABLE livetv_search_programmes USING fts5(source UNINDEXED, channel UNINDEXED, start UNINDEXED, body, tokenize='unicode61');
 CREATE TRIGGER livetv_search_programmes_insert AFTER INSERT ON livetv_programmes WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
 INSERT INTO livetv_search_programme_keys(source,channel,start) VALUES(new.source_id,new.channel_id,new.start_ms) ON CONFLICT DO NOTHING;
 INSERT OR REPLACE INTO livetv_search_programmes(rowid,source,channel,start,body) VALUES((SELECT docid FROM livetv_search_programme_keys WHERE source=new.source_id AND channel=new.channel_id AND start=new.start_ms),new.source_id,new.channel_id,new.start_ms,coalesce(json_extract(new.data,'$.item.title'),'') || ' ' || coalesce(json_extract(new.data,'$.item.episodeTitle'),'') || ' ' || coalesce(json_extract(new.data,'$.item.summary'),'')); END;
 CREATE TRIGGER livetv_search_programmes_delete AFTER DELETE ON livetv_programmes WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN DELETE FROM livetv_search_programmes WHERE rowid=(SELECT docid FROM livetv_search_programme_keys WHERE source=old.source_id AND channel=old.channel_id AND start=old.start_ms); DELETE FROM livetv_search_programme_keys WHERE source=old.source_id AND channel=old.channel_id AND start=old.start_ms; END;
 CREATE TRIGGER livetv_search_programmes_update AFTER UPDATE ON livetv_programmes WHEN (SELECT active FROM livetv_search_state WHERE id=1)=1 BEGIN
 DELETE FROM livetv_search_programmes WHERE rowid=(SELECT docid FROM livetv_search_programme_keys WHERE source=old.source_id AND channel=old.channel_id AND start=old.start_ms); DELETE FROM livetv_search_programme_keys WHERE source=old.source_id AND channel=old.channel_id AND start=old.start_ms; INSERT INTO livetv_search_programme_keys(source,channel,start) VALUES(new.source_id,new.channel_id,new.start_ms) ON CONFLICT DO NOTHING;
 INSERT OR REPLACE INTO livetv_search_programmes(rowid,source,channel,start,body) VALUES((SELECT docid FROM livetv_search_programme_keys WHERE source=new.source_id AND channel=new.channel_id AND start=new.start_ms),new.source_id,new.channel_id,new.start_ms,coalesce(json_extract(new.data,'$.item.title'),'') || ' ' || coalesce(json_extract(new.data,'$.item.episodeTitle'),'') || ' ' || coalesce(json_extract(new.data,'$.item.summary'),'')); END;
 CREATE INDEX livetv_search_name_fallback ON livetv_channels(lower(coalesce(json_extract(data,'$.name'),'')));
 CREATE INDEX livetv_search_number_fallback ON livetv_channels(lower(coalesce(json_extract(data,'$.number'),'')));
 CREATE INDEX livetv_search_channel_guide ON livetv_channels(coalesce(nullif(json_extract(data,'$.guideSourceId'),''),json_extract(data,'$.sourceId')),json_extract(data,'$.guideId'));
 `)
	store.RegisterDialect("livetv", 21, store.Postgres, `
 CREATE TABLE livetv_search_state(id INTEGER PRIMARY KEY, active INTEGER NOT NULL, ready INTEGER NOT NULL); INSERT INTO livetv_search_state VALUES(1,1,0);
 CREATE INDEX livetv_search_channels ON livetv_channels USING gin(to_tsvector('simple',coalesce(data::jsonb->>'name','') || ' ' || coalesce(data::jsonb->>'number','') || ' ' || coalesce(data::jsonb->>'guideId','')));
 CREATE INDEX livetv_search_programmes ON livetv_programmes USING gin(to_tsvector('simple',coalesce(data::jsonb->'item'->>'title','') || ' ' || coalesce(data::jsonb->'item'->>'episodeTitle','') || ' ' || coalesce(data::jsonb->'item'->>'summary','')));
 CREATE INDEX livetv_search_channel_guide ON livetv_channels(coalesce(nullif(data::jsonb->>'guideSourceId',''),data::jsonb->>'sourceId'),(data::jsonb->>'guideId'));
 `)
}

// SearchLiveTV searches identities once, regardless of guide overlap. Only a
// bounded candidate window is decoded and checked by the normal viewer policy.
func (h liveTVHost) SearchLiveTV(ctx context.Context, viewer *core.Viewer, query string, limit int) (frontend.LiveSearchResults, error) {
	out := frontend.LiveSearchResults{Channels: []frontend.LiveTVChannel{}, Programmes: []frontend.LiveTVProgramme{}}
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(words) == 0 || len(query) > 1024 || !h.s.Enabled() {
		return out, nil
	}
	if len(words) > 6 {
		words = words[:6]
	}
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	pg := store.DialectOf(h.s.o.DB) == store.Postgres
	if err := h.s.demandSearchIndex(ctx); err != nil {
		return out, err
	}
	plainWords := append([]string(nil), words...)
	h.s.searchMu.Lock()
	out.Partial = !h.s.searchReady
	h.s.searchMu.Unlock()
	for i, w := range words {
		if pg {
			words[i] = w + ":*"
		} else {
			words[i] = "\"" + w + "\"*"
		}
	}
	match := strings.Join(words, " AND ")
	if pg {
		match = strings.Join(words, " & ")
	}
	cq := `SELECT id FROM livetv_search_channels WHERE livetv_search_channels MATCH ? ORDER BY rank LIMIT 400`
	pq := `SELECT c.id,p.data FROM livetv_search_programmes f JOIN livetv_programmes p ON p.source_id=f.source AND p.channel_id=f.channel AND p.start_ms=f.start JOIN livetv_channels c ON coalesce(nullif(json_extract(c.data,'$.guideSourceId'),''),json_extract(c.data,'$.sourceId'))=p.source_id AND json_extract(c.data,'$.guideId')=p.channel_id WHERE livetv_search_programmes MATCH ? AND p.end_ms>? AND p.start_ms<? ORDER BY p.start_ms,c.id LIMIT 400`
	if pg {
		cq = `SELECT id FROM livetv_channels WHERE to_tsvector('simple',coalesce(data::jsonb->>'name','') || ' ' || coalesce(data::jsonb->>'number','') || ' ' || coalesce(data::jsonb->>'guideId','')) @@ to_tsquery('simple',?) ORDER BY id LIMIT 400`
		pq = `SELECT c.id,p.data FROM livetv_programmes p JOIN livetv_channels c ON coalesce(nullif(c.data::jsonb->>'guideSourceId',''),c.data::jsonb->>'sourceId')=p.source_id AND c.data::jsonb->>'guideId'=p.channel_id WHERE to_tsvector('simple',coalesce(p.data::jsonb->'item'->>'title','') || ' ' || coalesce(p.data::jsonb->'item'->>'episodeTitle','') || ' ' || coalesce(p.data::jsonb->'item'->>'summary','')) @@ to_tsquery('simple',?) AND p.end_ms>? AND p.start_ms<? ORDER BY p.start_ms,c.id LIMIT 400`
	}
	rows, err := h.s.o.DB.QueryContext(ctx, cq, match)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if re != nil {
		return out, re
	}
	if out.Partial && !pg {
		prefix := plainWords[0]
		fallback := `SELECT id FROM livetv_channels WHERE lower(coalesce(json_extract(data,'$.name'),''))>=? AND lower(coalesce(json_extract(data,'$.name'),''))<? UNION SELECT id FROM livetv_channels WHERE lower(coalesce(json_extract(data,'$.number'),''))>=? AND lower(coalesce(json_extract(data,'$.number'),''))<? LIMIT 400`
		fr, e := h.s.o.DB.QueryContext(ctx, fallback, prefix, prefix+"\uffff", prefix, prefix+"\uffff")
		if e != nil {
			return out, e
		}
		for fr.Next() {
			var id string
			if e = fr.Scan(&id); e != nil {
				break
			}
			ids = append(ids, id)
		}
		re := fr.Err()
		fr.Close()
		if e != nil {
			return out, e
		}
		if re != nil {
			return out, re
		}
	}
	now := h.s.o.Now()
	rows, err = h.s.o.DB.QueryContext(ctx, pq, match, now.UnixMilli(), now.Add(7*24*time.Hour).UnixMilli())
	if err != nil {
		return out, err
	}
	ps := []Programme{}
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var p Programme
		if err = json.Unmarshal(raw, &p); err != nil {
			break
		}
		p.ChannelID = id
		p.ID = fmt.Sprintf("%s-%d", id, p.Start.UnixMilli())
		ps = append(ps, p)
		ids = append(ids, id)
	}
	re = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if re != nil {
		return out, re
	}
	viewID := int64(0)
	if viewer != nil {
		viewID = viewer.ID
	}
	vq := `SELECT v.channel_id,v.data FROM livetv_search_virtual_fts f JOIN livetv_search_virtual v ON v.rowid=f.rowid WHERE livetv_search_virtual_fts MATCH ? AND v.end_ms>? AND v.start_ms<? AND (v.viewer_id=0 OR v.viewer_id=?) ORDER BY v.start_ms LIMIT 400`
	if pg {
		vq = `SELECT channel_id,data FROM livetv_search_virtual WHERE to_tsvector('simple',body) @@ to_tsquery('simple',?) AND end_ms>? AND start_ms<? AND (viewer_id=0 OR viewer_id=?) ORDER BY start_ms LIMIT 400`
	}
	vr, e := h.s.o.DB.QueryContext(ctx, vq, match, now.UnixMilli(), now.Add(7*24*time.Hour).UnixMilli(), viewID)
	if e != nil {
		return out, e
	}
	for vr.Next() {
		var id string
		var raw []byte
		if e = vr.Scan(&id, &raw); e != nil {
			break
		}
		var p Programme
		if e = json.Unmarshal(raw, &p); e != nil {
			break
		}
		p.ChannelID = id
		ps = append(ps, p)
		ids = append(ids, id)
	}
	re = vr.Err()
	vr.Close()
	if e != nil {
		return out, e
	}
	if re != nil {
		return out, re
	}
	unique := uniqueIDs(append([]string(nil), ids...))
	visible, err := h.s.searchVisibleGuideIDs(ctx, unique)
	if err != nil {
		return out, err
	}
	candidates, err := h.s.searchAllowedChannels(ctx, viewer, unique)
	if err != nil {
		return out, err
	}
	allowed := map[string]Channel{}
	for _, c := range candidates {
		if visible[c.ID] {
			allowed[c.ID] = c
		}
	}
	seen := map[string]bool{}
	for _, id := range ids[:len(ids)-len(ps)] {
		if c, ok := allowed[id]; ok && !seen[id] && len(out.Channels) < limit {
			if out.Partial {
				text := strings.ToLower(c.Name + " " + c.Number + " " + c.GuideID)
				matches := true
				for _, word := range plainWords {
					if !strings.Contains(text, word) {
						matches = false
						break
					}
				}
				if !matches {
					continue
				}
			}
			seen[id] = true
			out.Channels = append(out.Channels, ToFrontendChannel(c))
		}
	}
	channelIDs := map[string]bool{}
	ctx, _, err = h.s.withPolicy(ctx, viewer)
	if err != nil {
		return out, err
	}
	seen = map[string]bool{}
	for _, p := range ps {
		if c, ok := allowed[p.ChannelID]; ok && !seen[p.ID] && h.s.allowedProgramme(ctx, viewer, c, &p) && len(out.Programmes) < limit {
			seen[p.ID] = true
			out.Programmes = append(out.Programmes, toFrontendProgramme(p))
			if !channelIDs[c.ID] {
				channelIDs[c.ID] = true
				out.ProgrammeChannels = append(out.ProgrammeChannels, ToFrontendChannel(c))
			}
		}
	}
	return out, nil
}

// First use asks for preparation and returns immediately; never walk a large
// lineup in a search request. Backfill yields between small write transactions.
func (s *Service) demandSearchIndex(ctx context.Context) error {
	s.searchDemanded.Store(true)
	s.searchMu.Lock()
	defer s.searchMu.Unlock()
	if s.searchClosed {
		return core.ErrNotAvailable
	}
	if s.searchReady {
		return nil
	}
	var ready int
	if err := s.o.DB.QueryRowContext(ctx, `SELECT ready FROM livetv_search_state WHERE id=1`).Scan(&ready); err != nil {
		return err
	}
	if !s.searchBuilding {
		s.searchBuilding = true
		s.wg.Add(1)
		s.ctxMu.Lock()
		workerCtx := s.ctx
		s.ctxMu.Unlock()
		go func() {
			defer s.wg.Done()
			err := s.buildSearchIndex(workerCtx, ready == 0)
			s.searchMu.Lock()
			s.searchBuilding = false
			s.searchReady = err == nil
			s.searchMu.Unlock()
			if err != nil && !errors.Is(err, context.Canceled) {
				s.o.Logger.Warn("live search index preparation failed", "error", err)
			}
		}()
	}
	return nil
}
func (s *Service) buildSearchIndex(ctx context.Context, backfill bool) error {
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE livetv_search_state SET active=1 WHERE id=1`); err != nil {
		return err
	}
	tables := []string{}
	if backfill && store.DialectOf(s.o.DB) != store.Postgres {
		tables = []string{"livetv_channels", "livetv_programmes"}
	}
	for _, table := range tables {
		var last int64
		for {
			var end int64
			err := s.o.DB.QueryRowContext(ctx, `SELECT coalesce(max(rowid),0) FROM (SELECT rowid FROM `+table+` WHERE rowid>? ORDER BY rowid LIMIT 500)`, last).Scan(&end)
			if err != nil {
				return err
			}
			if end == 0 {
				break
			}
			keyQuery := `INSERT INTO livetv_search_channel_keys(id) SELECT id FROM livetv_channels WHERE rowid>? AND rowid<=? ON CONFLICT DO NOTHING`
			if table == "livetv_programmes" {
				keyQuery = `INSERT INTO livetv_search_programme_keys(source,channel,start) SELECT source_id,channel_id,start_ms FROM livetv_programmes WHERE rowid>? AND rowid<=? ON CONFLICT DO NOTHING`
			}
			batch, err := s.o.DB.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if _, err = batch.ExecContext(ctx, keyQuery, last, end); err != nil {
				batch.Rollback()
				return err
			}
			query := `INSERT OR REPLACE INTO livetv_search_channels(rowid,id,body) SELECT (SELECT docid FROM livetv_search_channel_keys k WHERE k.id=livetv_channels.id),id,json_extract(data,'$.name') || ' ' || coalesce(json_extract(data,'$.number'),'') || ' ' || coalesce(json_extract(data,'$.guideId'),'') FROM livetv_channels WHERE rowid>? AND rowid<=?`
			if table == "livetv_programmes" {
				query = `INSERT OR REPLACE INTO livetv_search_programmes(rowid,source,channel,start,body) SELECT (SELECT docid FROM livetv_search_programme_keys k WHERE k.source=livetv_programmes.source_id AND k.channel=livetv_programmes.channel_id AND k.start=livetv_programmes.start_ms),source_id,channel_id,start_ms,coalesce(json_extract(data,'$.item.title'),'') || ' ' || coalesce(json_extract(data,'$.item.episodeTitle'),'') || ' ' || coalesce(json_extract(data,'$.item.summary'),'') FROM livetv_programmes WHERE rowid>? AND rowid<=?`
			}
			if _, err = batch.ExecContext(ctx, query, last, end); err != nil {
				batch.Rollback()
				return err
			}
			if err = batch.Commit(); err != nil {
				return err
			}
			last = end
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	if err := s.backfillPersonalSearch(ctx); err != nil {
		return err
	}
	s.virtualGuide.mu.RLock()
	snapshots := make(map[string]*virtualGuideSnapshot, len(s.virtualGuide.entries))
	for id, entry := range s.virtualGuide.entries {
		snapshots[id] = entry
	}
	s.virtualGuide.mu.RUnlock()
	for id, entry := range snapshots {
		if err := s.indexVirtualSearchSnapshot(ctx, id, entry); err != nil {
			return err
		}
	}
	_, err := s.o.DB.ExecContext(ctx, `UPDATE livetv_search_state SET ready=1 WHERE id=1`)
	return err
}

func (s *Service) ownerSearchRoute(w http.ResponseWriter, r *http.Request) {
	result, err := (liveTVHost{s: s}).SearchLiveTV(r.Context(), &core.User{IsOwner: true}, r.URL.Query().Get("q"), 8)
	channels := []map[string]any{}
	programmes := []map[string]any{}
	for _, c := range result.Channels {
		channels = append(channels, map[string]any{"id": c.ID, "name": c.Name, "number": c.Number})
	}
	for _, p := range result.Programmes {
		programmes = append(programmes, map[string]any{"id": p.ID, "channelId": p.ChannelID, "title": p.Item.Title, "episodeTitle": p.Item.EpisodeTitle, "start": p.Start, "end": p.End})
	}
	ownerResult(w, map[string]any{"channels": channels, "programmes": programmes}, err)
}

// Search consumes only existing schedules. In particular a name hit must not
// prepare an unwatched virtual channel as a side effect of policy checking.
func (s *Service) searchAllowedChannels(ctx context.Context, viewer *core.Viewer, ids []string) ([]Channel, error) {
	ctx, policy, err := s.withPolicy(ctx, viewer)
	if err != nil {
		return nil, err
	}
	records, err := s.channelsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	cs := []Channel{}
	iptv := []Channel{}
	for _, id := range ids {
		if c, ok := records[id]; ok {
			cs = append(cs, c)
			if c.Kind != "virtual" {
				iptv = append(iptv, c)
			}
		}
	}
	now := s.o.Now()
	current, err := s.programmesAt(ctx, iptv, now)
	if err != nil {
		return nil, err
	}
	airing := map[string]Programme{}
	for i, p := range current {
		airing[iptv[i].ID] = p
	}
	out := []Channel{}
	for _, c := range cs {
		if !s.audienceAllowed(ctx, viewer, c) {
			continue
		}
		var p *Programme
		if c.Personal != nil && viewer != nil {
			var raw []byte
			e := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_search_virtual WHERE channel_id=? AND viewer_id=? AND start_ms<=? AND end_ms>? ORDER BY start_ms DESC LIMIT 1`, c.ID, viewer.ID, now.UnixMilli(), now.UnixMilli()).Scan(&raw)
			if e == nil {
				var current Programme
				if e = json.Unmarshal(raw, &current); e != nil {
					return nil, e
				}
				p = &current
			}
		} else if c.Kind == "virtual" {
			s.virtualGuide.mu.RLock()
			entry := s.virtualGuide.entries[c.ID]
			s.virtualGuide.mu.RUnlock()
			if entry != nil && entry.guide != nil {
				if ps := entry.guide.slice(c.ID, now, now.Add(time.Second)); len(ps) > 0 {
					p = &ps[0]
				}
			}
		} else if a, ok := airing[c.ID]; ok {
			p = &a
		}
		if p == nil && viewer != nil && (viewer.MaxContentRating != "" || policy.MaxRating != "") {
			continue
		}
		if s.allowedProgramme(ctx, viewer, c, p) {
			c.StreamURL = ""
			c.StreamSources = nil
			out = append(out, c)
		}
	}
	return out, nil
}

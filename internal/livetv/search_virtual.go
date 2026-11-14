package livetv

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/store"
	"strings"
	"time"
)

type virtualSearchIndexedSnapshot struct {
	programmes *compactProgrammes
	until      time.Time
}

const virtualSearchSchema = `CREATE TABLE livetv_search_virtual(docid INTEGER PRIMARY KEY,id TEXT NOT NULL,viewer_id BIGINT NOT NULL DEFAULT 0,channel_id TEXT NOT NULL,start_ms BIGINT NOT NULL,end_ms BIGINT NOT NULL,data TEXT NOT NULL,body TEXT NOT NULL,UNIQUE(id,viewer_id)); CREATE INDEX livetv_search_virtual_channel ON livetv_search_virtual(channel_id,viewer_id,start_ms,end_ms);`

func init() {
	store.Register("livetv", 22, virtualSearchSchema+`
 CREATE VIRTUAL TABLE livetv_search_virtual_fts USING fts5(body,tokenize='unicode61');
 CREATE TRIGGER livetv_search_virtual_insert AFTER INSERT ON livetv_search_virtual BEGIN INSERT OR REPLACE INTO livetv_search_virtual_fts(rowid,body) VALUES(new.rowid,new.body); END;
 CREATE TRIGGER livetv_search_virtual_delete AFTER DELETE ON livetv_search_virtual BEGIN DELETE FROM livetv_search_virtual_fts WHERE rowid=old.rowid; END;
 CREATE TRIGGER livetv_search_virtual_update AFTER UPDATE ON livetv_search_virtual BEGIN DELETE FROM livetv_search_virtual_fts WHERE rowid=old.rowid; INSERT OR REPLACE INTO livetv_search_virtual_fts(rowid,body) VALUES(new.rowid,new.body); END;
 CREATE TRIGGER livetv_search_virtual_channel_delete AFTER DELETE ON livetv_channels BEGIN DELETE FROM livetv_search_virtual WHERE channel_id=old.id; END;
 `)
	store.RegisterDialect("livetv", 22, store.Postgres, strings.Replace(virtualSearchSchema, "docid INTEGER PRIMARY KEY,", "docid BIGSERIAL PRIMARY KEY,", 1)+`CREATE INDEX livetv_search_virtual_text ON livetv_search_virtual USING gin(to_tsvector('simple',body));
 CREATE FUNCTION livetv_search_virtual_delete_channel() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN DELETE FROM livetv_search_virtual WHERE channel_id=old.id; RETURN old; END $$;
 CREATE TRIGGER livetv_search_virtual_channel_delete AFTER DELETE ON livetv_channels FOR EACH ROW EXECUTE FUNCTION livetv_search_virtual_delete_channel();`)
}

// Runs only after search has been used. Snapshot publication is already a
// background operation; indexing copies its materialised next 48 hours and
// never asks the scheduler, catalogue, or indexers to produce anything else.
func (s *Service) indexVirtualSearchSnapshot(ctx context.Context, id string, entry *virtualGuideSnapshot) error {
	if !s.searchDemanded.Load() || entry.guide == nil || entry.channel.Personal != nil {
		return nil
	}
	s.searchVirtualMu.Lock()
	defer s.searchVirtualMu.Unlock()
	s.virtualGuide.mu.RLock()
	current := s.virtualGuide.entries[id]
	s.virtualGuide.mu.RUnlock()
	if current != entry {
		return nil
	} // A newer snapshot won publication while we waited.
	if s.searchVirtualSnapshots == nil {
		s.searchVirtualSnapshots = map[string]virtualSearchIndexedSnapshot{}
	}
	now := s.o.Now()
	cached := s.searchVirtualSnapshots[id]
	if cached.programmes == entry.guide && cached.until.After(now.Add(24*time.Hour)) {
		return nil
	}
	ps := entry.guide.slice(id, now, now.Add(48*time.Hour))
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM livetv_search_virtual WHERE channel_id=? AND viewer_id=0`, id); err != nil {
		return err
	}
	for _, p := range ps {
		raw, e := json.Marshal(p)
		if e != nil {
			return e
		}
		body := strings.Join([]string{p.Item.Title, p.Item.EpisodeTitle, p.Item.Summary}, " ")
		if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_search_virtual(id,viewer_id,channel_id,start_ms,end_ms,data,body) VALUES(?,0,?,?,?,?,?)`, p.ID, id, p.Start.UnixMilli(), p.End.UnixMilli(), raw, body); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err == nil {
		s.searchVirtualSnapshots[id] = virtualSearchIndexedSnapshot{entry.guide, now.Add(48 * time.Hour)}
	}
	return err
}

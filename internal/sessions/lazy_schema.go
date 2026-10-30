package sessions

import "github.com/braedonsaunders/ether/internal/store"

func init() {
	store.RegisterAfter("sessions", "capability_headers")
	// Counters include expired rows until explicit deletion. Do not prune during
	// migration: an older node may still be transferring one of these capabilities.
	// Grandfather existing rows, but never allow an oversized store to grow.
	store.Register("sessions", 8, `
CREATE TABLE sessions_lazy_capacity (id INTEGER PRIMARY KEY, used BIGINT NOT NULL);
INSERT INTO sessions_lazy_capacity SELECT 1,COUNT(*) FROM sessions_lazy_streams;
CREATE TABLE sessions_lazy_viewer_capacity (viewer_id BIGINT PRIMARY KEY, used BIGINT NOT NULL);
INSERT INTO sessions_lazy_viewer_capacity SELECT viewer_id,COUNT(*) FROM sessions_lazy_streams GROUP BY viewer_id;
CREATE INDEX sessions_lazy_viewer ON sessions_lazy_streams(viewer_id);
INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires)
 SELECT 'lazy-stream',hash,viewer_id%64,viewer_id,protocol,'',expires FROM sessions_lazy_streams;
CREATE TRIGGER sessions_lazy_insert AFTER INSERT ON sessions_lazy_streams BEGIN
 UPDATE sessions_lazy_capacity SET used=used+1 WHERE id=1;
 INSERT INTO sessions_lazy_viewer_capacity(viewer_id,used) VALUES(NEW.viewer_id,1)
 ON CONFLICT(viewer_id) DO UPDATE SET used=used+1;
 SELECT CASE WHEN (SELECT used FROM sessions_lazy_capacity WHERE id=1)>8192
 OR (SELECT used FROM sessions_lazy_viewer_capacity WHERE viewer_id=NEW.viewer_id)>256
 OR length(CAST(NEW.request AS BLOB))>65536 THEN RAISE(ABORT,'lazy capability capacity exceeded') END;
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM capability_headers WHERE namespace='lazy-stream' AND digest=NEW.hash AND viewer_id=NEW.viewer_id AND surface=NEW.protocol AND device_id='' AND expires=NEW.expires) THEN RAISE(ABORT,'lazy capability admission required') END;
END;
CREATE TRIGGER sessions_lazy_delete AFTER DELETE ON sessions_lazy_streams BEGIN
 SELECT CASE WHEN EXISTS(SELECT 1 FROM capability_headers WHERE namespace='lazy-stream' AND digest=OLD.hash) THEN RAISE(ABORT,'lazy capability retirement required') END;
 UPDATE sessions_lazy_capacity SET used=used-1 WHERE id=1;
 UPDATE sessions_lazy_viewer_capacity SET used=used-1 WHERE viewer_id=OLD.viewer_id;
 DELETE FROM sessions_lazy_viewer_capacity WHERE viewer_id=OLD.viewer_id AND used=0;

END;
CREATE TRIGGER sessions_lazy_update BEFORE UPDATE ON sessions_lazy_streams BEGIN
 SELECT CASE WHEN NEW.viewer_id<>OLD.viewer_id OR NEW.protocol<>OLD.protocol OR NEW.hash<>OLD.hash OR NEW.grant_reference<>OLD.grant_reference OR NEW.request<>OLD.request
 OR length(CAST(NEW.request AS BLOB))>65536 THEN RAISE(ABORT,'lazy capability scope immutable') END;
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM capability_headers WHERE namespace='lazy-stream' AND digest=NEW.hash AND viewer_id=NEW.viewer_id AND surface=NEW.protocol AND device_id='' AND expires=NEW.expires) THEN RAISE(ABORT,'lazy capability admission required') END;
END;`)
	store.RegisterDialect("sessions", 8, store.Postgres, `
CREATE TABLE sessions_lazy_capacity (id INTEGER PRIMARY KEY, used BIGINT NOT NULL);
INSERT INTO sessions_lazy_capacity SELECT 1,COUNT(*) FROM sessions_lazy_streams;
CREATE TABLE sessions_lazy_viewer_capacity (viewer_id BIGINT PRIMARY KEY, used BIGINT NOT NULL);
INSERT INTO sessions_lazy_viewer_capacity SELECT viewer_id,COUNT(*) FROM sessions_lazy_streams GROUP BY viewer_id;
CREATE INDEX sessions_lazy_viewer ON sessions_lazy_streams(viewer_id);
INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires)
 SELECT 'lazy-stream',hash,viewer_id%64,viewer_id,protocol,'',expires FROM sessions_lazy_streams;
CREATE FUNCTION sessions_lazy_account() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE total_used BIGINT; viewer_used BIGINT;
BEGIN
 IF TG_OP='INSERT' THEN
  UPDATE sessions_lazy_capacity SET used=used+1 WHERE id=1 RETURNING used INTO total_used;
  INSERT INTO sessions_lazy_viewer_capacity(viewer_id,used) VALUES(NEW.viewer_id,1)
  ON CONFLICT(viewer_id) DO UPDATE SET used=sessions_lazy_viewer_capacity.used+1 RETURNING used INTO viewer_used;
  IF total_used>8192 OR viewer_used>256 OR octet_length(NEW.request)>65536 THEN
   RAISE EXCEPTION 'lazy capability capacity exceeded';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM capability_headers WHERE namespace='lazy-stream' AND digest=NEW.hash AND viewer_id=NEW.viewer_id AND surface=NEW.protocol AND device_id='' AND expires=NEW.expires) THEN RAISE EXCEPTION 'lazy capability admission required'; END IF;
  RETURN NEW;
 ELSIF TG_OP='DELETE' THEN
  IF EXISTS(SELECT 1 FROM capability_headers WHERE namespace='lazy-stream' AND digest=OLD.hash) THEN RAISE EXCEPTION 'lazy capability retirement required'; END IF;
  UPDATE sessions_lazy_capacity SET used=used-1 WHERE id=1;
  UPDATE sessions_lazy_viewer_capacity SET used=used-1 WHERE viewer_id=OLD.viewer_id;
  DELETE FROM sessions_lazy_viewer_capacity WHERE viewer_id=OLD.viewer_id AND used=0;

  RETURN OLD;
 ELSE
  IF NEW.viewer_id<>OLD.viewer_id OR NEW.protocol<>OLD.protocol OR NEW.hash<>OLD.hash OR NEW.grant_reference<>OLD.grant_reference OR NEW.request<>OLD.request OR octet_length(NEW.request)>65536 THEN
   RAISE EXCEPTION 'lazy capability scope immutable';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM capability_headers WHERE namespace='lazy-stream' AND digest=NEW.hash AND viewer_id=NEW.viewer_id AND surface=NEW.protocol AND device_id='' AND expires=NEW.expires) THEN RAISE EXCEPTION 'lazy capability admission required'; END IF;
  RETURN NEW;
 END IF;
END $$;
CREATE TRIGGER sessions_lazy_insert AFTER INSERT ON sessions_lazy_streams FOR EACH ROW EXECUTE FUNCTION sessions_lazy_account();
CREATE TRIGGER sessions_lazy_delete AFTER DELETE ON sessions_lazy_streams FOR EACH ROW EXECUTE FUNCTION sessions_lazy_account();
CREATE TRIGGER sessions_lazy_update BEFORE UPDATE ON sessions_lazy_streams FOR EACH ROW EXECUTE FUNCTION sessions_lazy_account();`)
}

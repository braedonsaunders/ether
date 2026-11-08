package catalog

import (
	"github.com/braedonsaunders/ether/internal/store"
)

// catalog_show_episode_ratings splits catalog_show_episodes by the episode
// facts a viewer's rating cap and plex.tv share filter read: the episode's
// own content rating, its rank and its collection name (an empty rating
// inherits the series'). A restricted viewer's whole-library episode total
// is then a sum over the admitted series' few rows, as an unrestricted one
// is, instead of a join over every stored episode. Triggers keep it in the
// writing transaction, per series like catalog_show_episodes.

func episodeRatingsMember(alias string) string {
	return alias + ".type='episode' AND " + alias + ".adult=0 AND " + alias + ".released_ts<>0 AND " + alias + ".show_key<>0"
}

var showEpisodeRatingsBackfill = `INSERT INTO catalog_show_episode_ratings(show_key,content_rating,rating_rank,collection_name,known)
 SELECT e.show_key,e.content_rating,e.content_rating_rank,e.collection_name,COUNT(*) FROM catalog_titles e WHERE ` + episodeRatingsMember("e") + `
 GROUP BY e.show_key,e.content_rating,e.content_rating_rank,e.collection_name`

func init() {
	upsert := func(alias string) string {
		return `INSERT INTO catalog_show_episode_ratings(show_key,content_rating,rating_rank,collection_name,known) VALUES (` + alias + `.show_key,` + alias + `.content_rating,` + alias + `.content_rating_rank,` + alias + `.collection_name,1)
  ON CONFLICT(show_key,content_rating,rating_rank,collection_name) DO UPDATE SET known=catalog_show_episode_ratings.known+1;`
	}
	decrement := func(alias string) string {
		return `UPDATE catalog_show_episode_ratings SET known=known-1 WHERE show_key=` + alias + `.show_key AND content_rating=` + alias + `.content_rating AND rating_rank=` + alias + `.content_rating_rank AND collection_name=` + alias + `.collection_name;`
	}
	columns := `type, adult, released_ts, show_key, content_rating, content_rating_rank, collection_name`
	changed := `(` + episodeRatingsMember("old") + `) IS NOT (` + episodeRatingsMember("new") + `) OR old.show_key IS NOT new.show_key OR old.content_rating IS NOT new.content_rating OR old.content_rating_rank IS NOT new.content_rating_rank OR old.collection_name IS NOT new.collection_name`
	schema := `CREATE TABLE catalog_show_episode_ratings(show_key INTEGER NOT NULL, content_rating TEXT NOT NULL, rating_rank INTEGER NOT NULL, collection_name TEXT NOT NULL, known INTEGER NOT NULL, PRIMARY KEY(show_key,content_rating,rating_rank,collection_name));
` + showEpisodeRatingsBackfill + `;`
	store.Register("catalog", 45, schema+`
CREATE TRIGGER catalog_show_episode_ratings_insert AFTER INSERT ON catalog_titles
WHEN `+episodeRatingsMember("new")+` BEGIN `+upsert("new")+` END;
CREATE TRIGGER catalog_show_episode_ratings_delete AFTER DELETE ON catalog_titles
WHEN `+episodeRatingsMember("old")+` BEGIN `+decrement("old")+` END;
CREATE TRIGGER catalog_show_episode_ratings_update AFTER UPDATE OF `+columns+` ON catalog_titles
WHEN `+changed+` BEGIN
 UPDATE catalog_show_episode_ratings SET known=known-1 WHERE show_key=old.show_key AND content_rating=old.content_rating AND rating_rank=old.content_rating_rank AND collection_name=old.collection_name AND `+episodeRatingsMember("old")+`;
 INSERT INTO catalog_show_episode_ratings(show_key,content_rating,rating_rank,collection_name,known)
  SELECT new.show_key,new.content_rating,new.content_rating_rank,new.collection_name,1 WHERE `+episodeRatingsMember("new")+`
  ON CONFLICT(show_key,content_rating,rating_rank,collection_name) DO UPDATE SET known=catalog_show_episode_ratings.known+1;
END;`)
	store.RegisterDialect("catalog", 45, store.Postgres, `LOCK TABLE catalog_titles IN SHARE ROW EXCLUSIVE MODE;
CREATE TABLE catalog_show_episode_ratings(show_key BIGINT NOT NULL, content_rating TEXT NOT NULL, rating_rank BIGINT NOT NULL, collection_name TEXT NOT NULL, known BIGINT NOT NULL, PRIMARY KEY(show_key,content_rating,rating_rank,collection_name));
`+showEpisodeRatingsBackfill+`;
CREATE FUNCTION catalog_show_episode_ratings_count() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP IN ('UPDATE','DELETE') AND `+episodeRatingsMember("OLD")+` THEN `+decrement("OLD")+` END IF;
 IF TG_OP IN ('INSERT','UPDATE') AND `+episodeRatingsMember("NEW")+` THEN `+upsert("NEW")+` END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER catalog_show_episode_ratings_count AFTER INSERT OR DELETE ON catalog_titles
 FOR EACH ROW EXECUTE FUNCTION catalog_show_episode_ratings_count();
CREATE TRIGGER catalog_show_episode_ratings_update AFTER UPDATE OF `+columns+` ON catalog_titles
 FOR EACH ROW WHEN (`+pgDistinct(changed)+`) EXECUTE FUNCTION catalog_show_episode_ratings_count();`)
}

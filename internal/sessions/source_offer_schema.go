package sessions

import "github.com/braedonsaunders/ether/internal/store"

const sourceOfferTable = `CREATE TABLE sessions_source_offers(
 id INTEGER PRIMARY KEY AUTOINCREMENT CHECK(id>2147483647 AND id<=9007199254740991),
 namespace TEXT NOT NULL CHECK(namespace='sessions.source-offer'),
 digest TEXT NOT NULL,
 item_id BIGINT NOT NULL CHECK(item_id>0),
 kind TEXT NOT NULL CHECK(kind IN ('version','detail')),
 reference TEXT NOT NULL,
 revision TEXT NOT NULL,
 CHECK((kind='version' AND revision='') OR (kind='detail' AND revision<>'')),
 UNIQUE(namespace,digest),
 FOREIGN KEY(namespace,digest) REFERENCES capability_headers(namespace,digest) ON DELETE CASCADE
); CREATE INDEX sessions_source_offers_target ON sessions_source_offers(item_id,kind,reference,revision);`

func init() {
	store.RegisterAfter("sessions_source_offers", "sessions", "capability_headers")
	store.Register("sessions_source_offers", 1, sourceOfferTable+`INSERT INTO sqlite_sequence(name,seq) VALUES('sessions_source_offers',2147483647);`)
	store.RegisterDialect("sessions_source_offers", 1, store.Postgres, `CREATE TABLE sessions_source_offers(
 id BIGINT GENERATED ALWAYS AS IDENTITY (START WITH 2147483648 MAXVALUE 9007199254740991 NO CYCLE) PRIMARY KEY CHECK(id>2147483647 AND id<=9007199254740991),
 namespace TEXT NOT NULL CHECK(namespace='sessions.source-offer'),digest TEXT NOT NULL,item_id BIGINT NOT NULL CHECK(item_id>0),
 kind TEXT NOT NULL CHECK(kind IN ('version','detail')),reference TEXT NOT NULL,revision TEXT NOT NULL,
 CHECK((kind='version' AND revision='') OR (kind='detail' AND revision<>'')),UNIQUE(namespace,digest),
 FOREIGN KEY(namespace,digest) REFERENCES capability_headers(namespace,digest) ON DELETE CASCADE
 );CREATE INDEX sessions_source_offers_target ON sessions_source_offers(item_id,kind,reference,revision);`)
}

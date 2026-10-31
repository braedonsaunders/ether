package playback

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
	"time"
)

func TestObservedMediaPreservesIdentityAndViewerScope(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &resumeStore{db: db}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	key := core.MovieKey(7)
	info := core.MediaInfo{Size: 500, Duration: time.Hour, Streams: []core.StreamInfo{
		{Type: "video", Height: 2160, Codec: "hevc", DolbyVision: true},
		{Type: "subtitle", External: true, ExternalURL: "https://fixture.invalid/secret"},
	}}
	raw, _ := json.Marshal(probeFactsRecord{FactsVersion: core.ProbeFactsVersion, Info: info})
	_, err = db.ExecContext(ctx, `INSERT INTO playback_resume(user_id,rating_key,release_id,release_json,nzb_hash,size,updated_at) VALUES(1,?,'r','{}','hash',500,?)`, key, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO playback_probe(user_id,rating_key,release_id,nzb_hash,size,info_json,updated_at) VALUES(1,?,'r','hash',500,?,?)`, key, string(raw), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.observedMedia(ctx, 1, key, now)
	if err != nil || got == nil || len(got.Info.Streams) != 1 || got.Info.Video().Height != 2160 {
		t.Fatalf("exact evidence lost: %+v %v", got, err)
	}
	if got, _ := s.observedMedia(ctx, 2, key, now); got != nil {
		t.Fatal("another viewer's probe exposed")
	}
	if got, _ := s.observedMedia(ctx, 1, key, now.Add(31*24*time.Hour)); got != nil {
		t.Fatal("expired facts exposed")
	}
	for _, tc := range []struct{ name, sql string }{
		{"release", `UPDATE playback_resume SET release_id='other'`},
		{"hash", `UPDATE playback_resume SET nzb_hash='other'`},
		{"size", `UPDATE playback_resume SET size=501`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := db.ExecContext(ctx, tc.sql); e != nil {
				t.Fatal(e)
			}
			if got, e := s.observedMedia(ctx, 1, key, now); e != nil || got != nil {
				t.Fatalf("mismatch accepted %+v %v", got, e)
			}
			if _, e := db.ExecContext(ctx, `UPDATE playback_resume SET release_id='r',nzb_hash='hash',size=500`); e != nil {
				t.Fatal(e)
			}
		})
	}
	for _, bad := range []string{`broken`, `{"estimated":true,"size":500,"duration":1000,"streams":[{"type":"video","height":2160}]}`, `{"size":501,"duration":1000,"streams":[{"type":"video","height":2160}]}`} {
		if _, e := db.ExecContext(ctx, `UPDATE playback_probe SET info_json=?`, bad); e != nil {
			t.Fatal(e)
		}
		if got, e := s.observedMedia(ctx, 1, key, now); e != nil || got != nil {
			t.Fatalf("corrupt/estimated facts accepted %+v %v", got, e)
		}
	}
}

func TestObservedMediaReadsOnlyMatchingActiveExactSession(t *testing.T) {
	now := time.Now()
	key := core.MovieKey(7)
	mk := func(id string, user int64, height int, estimated bool) *session {
		s := newSession(id, identity{userID: user, key: key}, &core.User{ID: user}, core.ClientCaps{}, &core.Title{Key: key}, "", now)
		s.cur = &generation{info: core.MediaInfo{Size: 500, Duration: time.Hour, Estimated: estimated, Streams: []core.StreamInfo{{Type: "video", Height: height}}}}
		t.Cleanup(s.stopCancel)
		return s
	}
	m := &Manager{now: func() time.Time { return now }, sessions: map[string]*session{
		"one": mk("one", 1, 1080, false), "two": mk("two", 2, 2160, false), "estimated": mk("estimated", 3, 2160, true),
	}}
	got, err := m.ObservedMediaInfo(context.Background(), 1, key)
	if err != nil || got == nil || got.Info.Video().Height != 1080 {
		t.Fatalf("wrong active facts: %+v %v", got, err)
	}
	if got, _ := m.ObservedMediaInfo(context.Background(), 3, key); got != nil {
		t.Fatal("active estimate exposed")
	}
	if got, _ := m.ObservedMediaInfo(context.Background(), 4, key); got != nil {
		t.Fatal("another viewer's active facts exposed")
	}
}

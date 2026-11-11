package plex

import "testing"

// Issue #410: a Plex client that went silent (app backgrounded) kept its
// encoder and source sessions because now-playing expiry only dropped the
// display entry. Expiring a playing entry must end playback like a stopped
// timeline; a paused entry is left to its retention clocks.
func TestExpiredPlayingEntryEndsPlayback(t *testing.T) {
	for _, state := range []string{"playing", "paused"} {
		t.Run(state, func(t *testing.T) {
			f, m := issue94Fixture(t, true, "open")
			tr := f.srv.transcoder().(*issue94Transcoder)
			h := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "play-410", "Accept", "application/json"}
			for _, path := range []string{"decision?", "start.m3u8?"} {
				if r := f.get(t, "/video/:/transcode/universal/"+path+issue94Query("trans-410", 3000), h...); r.Code != 200 {
					t.Fatalf("%s %d", path, r.Code)
				}
			}
			if r := f.get(t, "/:/timeline?ratingKey=1000000000000550&state="+state+"&time=1000", h...); r.Code != 200 {
				t.Fatalf("timeline %d", r.Code)
			}
			entries := f.srv.sessions.list()
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			e := entries[0]
			f.srv.sessions.remove(e.id) // as the janitor's expire did
			f.srv.endExpiredEntry(e)
			_, encoderAlive := tr.Session("trans-410")
			_, sourceAlive := m.SessionFor(ownerUser.ID, "phone", fightClub.Key)
			if state == "paused" {
				if !encoderAlive || !sourceAlive {
					t.Fatal("paused entry expiry ended a retained session")
				}
				return
			}
			if encoderAlive {
				t.Fatal("expired playing entry left the encoder running")
			}
			if sourceAlive {
				t.Fatal("expired playing entry left the source session running")
			}
		})
	}
}

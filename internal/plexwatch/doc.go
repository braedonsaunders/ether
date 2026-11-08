// Package plexwatch syncs one viewer's watched marks, resume positions and
// ratings with that viewer's own Plex account, and (pms*.go) with Plex
// Media Servers the owner runs.
//
// # Account sync
//
// Transport: discover.provider.plex.tv, the calls Plex Web makes for "Mark as
// watched" on Discover, verified against a real account on 2026-11-07:
//
//   - GET /library/metadata/{k1,k2,...}/userState answers MediaContainer.
//     UserState (an object for one key, an array for several) with ratingKey,
//     type, viewCount, viewOffset (ms), viewState ("progress", or "ignore"
//     once removed from Continue Watching), lastViewedAt (unix seconds),
//     userRating (1–10) and, for shows, viewedLeafCount. Titles with no
//     state are omitted. Mixed movie/show/episode batches work; reads are
//     capped at maxStateBatch keys.
//   - PUT /actions/scrobble, /actions/unscrobble, /actions/rate (rating=1–10,
//     -1 clears) and /actions/removeFromContinueWatching, each with
//     identifier=tv.plex.provider.discover&key={key}. They are idempotent.
//     Scrobble and unscrobble reset viewOffset; unscrobble keeps
//     lastViewedAt, so "watched" is viewCount>0, never lastViewedAt alone.
//     removeFromContinueWatching sets viewState "ignore" and keeps
//     viewOffset.
//   - GET /timeline?ratingKey={key}&key=/library/metadata/{key}&state=stopped
//     &time={ms}&duration={ms} sets the resume position (PUT/POST answer
//     404; no duration answers 400). It moves lastViewedAt, sets viewState
//     back to "progress", and never marks a title watched, even past 90%:
//     Ether's own threshold decides and sends a scrobble. An episode's
//     timeline does not move its show's userState.
//
// {key} is the hex id inside a title's verified plex://movie|show|episode GUID
// (internal/plexguid). Titles without a verified GUID are never synced and no
// other identity matching is attempted.
//
// Credential: the viewer's own Plex account token from the PIN sign-in
// (POST/GET plex.tv/api/v2/pins), stored sealed per viewer and sent only in
// X-Plex-Token. The PIN must be approved by the Plex account already recorded
// for that viewer; the owner's token is never used for a viewer. A 401 or
// 403 from any call erases the token and marks the connection for
// reconnecting.
//
// Consent: the account's "Sync My Watch State and Ratings" setting, GET
// plex.tv/api/v2/user/view_state_sync ({"consent":bool}). Plex does not
// enforce it on the Discover routes, so Ether does: while it is off nothing
// is imported and nothing is sent. It is read once per viewer per pass,
// never per title: at the start of every scheduled pass (which also runs,
// at the viewer's interval, for viewers who only send while it is off) and
// before the first send of each delivery pass. While it is off the
// viewer's queued changes are parked, not dropped; the first read that
// finds it on wakes them, still subject to read-back and newest-wins.
//
// Ether → Plex: watch changes write durable intents to state_sync_journal in
// the watch transaction, stamped with the viewer's fence generation. The
// sender claims a row, resolves the GUID, and admits it under the fence lock
// (generation unchanged, sending still on) before the request leaves.
// Turning sending off, disconnecting and reconnecting move the fence under
// the same lock, so nothing is admitted afterwards; a request admitted just
// before cannot be recalled. Admission sets never_sent=0. When an attempt
// ends without a definitive answer (5xx, timeout, crash), the next attempt
// reads userState first: if Plex already holds the change it is done, if
// Plex shows a newer change it yields, otherwise it is sent again. 429 pauses
// the viewer for Retry-After; other failures back off exponentially.
//
// Resume positions are a third journal aspect. A position reported during
// playback is held for state.OffsetHold and later positions of the title
// replace it in place, so playback sends at most one position per hold;
// pause and stop send the latest at once. A watch change supersedes a
// queued position, and positions share the title's watch lock, so a stale
// position never reaches Plex after a completion or unwatch. Clearing a
// position (a rewind to the start) is read back first and sent as an
// unscrobble only while Plex has the title unwatched. A position Ether
// has no duration for is not queued (Plex refuses it).
//
// Continue Watching removals are journaled in the same aspect, one per
// in-progress movie or episode the removal covers (a show covers each of
// its episodes with a position), carrying Ether's position. A removal
// replaces a queued position in place and a later position replaces a
// removal, as playback brings a title back. It is read back first: if Plex
// played the title since, it yields; if Plex lacks the position, the
// position is sent before removeFromContinueWatching.
//
// Plex → Ether: a leader-only, bounded background pass per viewer reads
// userState in batches over Ether's verified movie and show identities,
// resuming from a key cursor. Shows whose watched-episode count or last view
// moved queue their episodes for reading. Because an episode's position
// does not move its show, each sweep is followed by a pass over the
// viewer's active shows (any watch, progress or rating activity in Ether
// within 180 days, plus the viewer's watchlist), reading their verified
// episodes in batches from a show cursor; its cost follows the viewer's
// activity, never the library. plexwatch_remote keeps what was last read
// per title, so only changes since then apply: a Plex view or resume
// position by its lastViewedAt against the local watch clock; an unwatch,
// cleared position, Continue Watching removal or rating change, which Plex
// does not timestamp, only when Ether has not changed that title since the
// previous read. A watch change read together with a position wins over
// it. A Plex removal of a title Ether has in progress removes it in Ether
// and keeps the resume point; a Plex playback newer than the viewer's
// removal in Ether brings the title back. Imported changes are account
// events, which are never journaled, and values Ether wrote itself are
// adopted rather than applied, so nothing echoes in either direction.
package plexwatch

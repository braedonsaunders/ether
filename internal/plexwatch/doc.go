// Package plexwatch delivers one viewer's watched state, and whole-star
// ratings, to that viewer's own Plex account.
//
// Local watch rows stay authoritative. A watch mark and a rating each have
// their own timestamp, so one cannot make the other look newer. Intents are
// rows in state_sync_journal, written in the same transaction as the watch
// change. This package does not subscribe to in-memory state events.
//
// Provenance of the upstream contract, none of it a live call from this
// repository:
//
//   - POST https://scrobbles.plex.tv/state returned 201 for items with
//     guid, viewedAt (unix milliseconds), viewState "complete" or "unplayed",
//     optional isBulk, and optional ratedAt plus an integer rating.
//     The user field is the numeric Plex account id. The GET echo is
//     GET https://scrobbles.plex.tv/state/{accountId}?after={cursor}&count=100
//     with items and pageInfo.endCursor / hasNextPage. isBulk and a show-level
//     guid in the POST were omitted from that echo.
//     Source: PMS verbose log posted by doug316, 2024-06-07,
//     https://forums.plex.tv/t/mark-as-unwatched-does-not-work/879522
//
//   - viewState "complete" for a watched movie, a rating-only item
//     {"guid","ratedAt","rating"}, and HTTP 422 when viewedAt is in the
//     future (the whole POST fails). Plex employee gbooker02 identified the
//     future timestamp. Source: Havohej, 2024-01-24,
//     https://forums.plex.tv/t/status-sync-for-old-movies-and-series/867157
//
//   - A custom-agent GUID received HTTP 400 and PMS stopped that sync.
//     Plex employee BigWheel attributed it to the custom agent.
//     Source: ItsThatJA, 2026-03-19,
//     https://forums.plex.tv/t/can-these-reviews-for-my-pfofile-be-deleted-by-a-plex-dev/937027
//
//   - PMS decides whether to sync with
//     GET https://plex.tv/api/v2/server/users/{accountId}/view_state_sync
//     (200 continue, 403 stop). It obtains per-user tokens from
//     GET https://plex.tv/api/v2/server/users/services?auth_token=<server token>.
//     This package does not call the services route and does not send a
//     server, shared-server, or home-switch token.
//     Sources: https://forums.plex.tv/t/unable-to-install-new-plex-server-on-new-w-11-computer/934457
//     and https://forums.plex.tv/t/403-response-from-get-https-plex-tv-api-v2-server-users-xxx-view-state-sync-reused/883818
//
//   - Official scope: movies and episodes only, no in-progress offsets, no
//     music, no movie editions. Activity and profile history depend on the
//     account setting "Sync My Watch State and Ratings" and on privacy
//     settings. A POST does not by itself guarantee a Discover Activity card.
//     https://support.plex.tv/articles/sync-watch-state-and-ratings/ (modified 2025-03-24)
//
//   - A Plex engineer said metadata.provider.plex.tv scrobble handlers are
//     deprecated. This package does not call them.
//     https://github.com/Taxel/PlexTraktSync/discussions/1842
//
// The token stored here is the authToken from POST/GET /api/v2/pins, the same
// credential Ether already uses for GET /api/v2/user. Captured ViewStateSync
// logs do not show that a PIN token, rather than a services token, is what
// scrobbles.plex.tv accepts. Eligibility is therefore a read of
// view_state_sync with that stored token, and a refusal leaves sync off.
// The check is a GET. A result is stored only for the token generation that
// was checked, so a late response cannot restore a revoked token or change
// the account that replaced it. This package does not PUT consent and does
// not call /api/v2/server/users/services or /api/home. ProbeToken is that
// read-only pair of requests and does not start a PIN.
//
// Permit admission and consent changes (enable, disable, reconnect, revoke)
// take the same fence-row lock. Admission inserts a permit only when that
// locked generation matches the intent's stored consent generation and this
// attempt still owns the live journal row. A database error reading that
// generation leaves the intent retryable. A confirmed missing or stale
// generation does not send.
//
// A permit already inserted is in flight. Turning sync off does not delete
// it, and a request already handed to Plex cannot be recalled. An intent
// that has not been admitted is not sent after the fence moves, including
// when lookup began under the previous generation. Releasing one permit
// does not change another. A journal row records the fence generation
// observed when it was written; a row that commits after a disable keeps
// that older generation and is not sent when sync is turned on again.
package plexwatch

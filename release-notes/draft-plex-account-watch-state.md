# Draft: Plex account watch state

This file is not a release. It is not named as a version tag and must not be
published until the feature is in the commit being tagged.

Issue #143, including comment 6526097300, is a request to show watch state on
the Plex account. Ether's own history already records the plays. This is not
a local-state bug. The integration sends watched and unwatched marks to the
account. It does not promise a Discover Activity card.

Optional, per viewer, and off until that viewer connects their own Plex
account and turns on future sync. Ether can then send watched and unwatched
marks, and whole-star ratings of 1 through 5, to `scrobbles.plex.tv`. Playback
progress stays on this server. Existing history is not uploaded unless that
viewer uses Export, and Plex history is not copied in unless they use Import.

Connecting the owner's account to a guest is rejected. The account that
approves the sign-in code must be that viewer. Ether does not use the server
token or shared-account tokens for this sync, and it does not turn on Plex's
own watch-state setting.

Limitations that are still true:

- A live account-token write to `scrobbles.plex.tv`, and a live eligibility
  read, have not been proven. If Plex refuses the token, sync stays off.
- A successful send does not create a Discover Activity card.
- Clears, fractions, and ratings other than 2, 4, 6, 8, and 10 stay local.
- Episodes are sent only when an exact `plex://episode` id is already stored.
  Show and season ids are not expanded.
- Neither a Plex failure nor a missing id blocks local playback.
- Delivery is outbound only. Import is manual. A send is admitted under the
  same fence lock as turning sync off. Sync that is not yet admitted is not
  sent after a disable or a new sign-in, even if title lookup already
  started. A permit already admitted stays in flight: turning sync off does
  not delete it, and a request already handed to Plex cannot be recalled.
  The page says when one is still in progress.
- Titles Plex knows before this server has mapped them are kept and can be
  retried. A database error stops that import instead of skipping ahead.
- Old delivered journal rows, idle locks, and echo records are compacted.
  The newest undelivered intent and the newest echo rows are kept. Viewers
  who have not turned sync on are not journaled.

Schema integration. Migrations apply by component name, then version. This
tree has no RegisterAfter. The account-sync additions are component
state_plexwatch, which sorts after state: /1 journal and lock, /2 watch_at
and rating_at, /3 consent_generation. plexwatch_state would sort before
state, and the clock ALTER would run before state_watch exists. Account
tables stay on component plexwatch (/1 through /4). state/1 and state/2 are
unchanged.

The origin/prism tip fetched for this check was
b4ef791d53a68741c0f1513071328594b62eb9ed (2026-10-30 09:26 -0400, "Clarify
row-view guide cache scope"). That is the commit whose SQL was read. It is
not a claim that the Prism coordinator's live head is still that commit.
On that snapshot, state/3 is state_playlist_markers and state/4 is
state_last_source plus state_source_sequence. There is no state/5 and no
plexwatch component. A database that applies both trees records Prism's
state/3 and state/4 and then state_plexwatch/1 through /3. Fresh OpenTest
is not that proof. TestFreshStatePlexwatchSchema, TestOldBinaryWriteSurvivesUpgrade,
TestDevOnlyUnreleasedIdentityConverts, and TestPrismStateAndPlexwatchSchema
cover the empty database, the state/2 upgrade, a dev-only rewrite of the
unreleased state/3-5 bookkeeping, and the combined schema. Startup does not
run that rewrite, does not refuse a newer schema, and does not restore or
discard writes.

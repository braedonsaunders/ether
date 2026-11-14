# Quality ranking: the Radarr/Sonarr standard, streamed

Status: v0.3.6, built on evulhotdog's PR #355 (per-profile Servarr
evaluation).

## Principle

Radarr and Sonarr decide what to **download and keep**. Ether decides what
to **stream to this device right now**. Ether uses their ranking standard
as is (one quality order, custom formats with scores, the same comparer).
Ether adds only what a download manager can't know: the device, the
viewer, the connection and whether a source plays now. That context is a
set of filters and gates in front of the standard order, plus a few
built-in custom formats that score like any other format. It is not a
second ranking ladder.

There is one model for every profile. The per-profile "Ranking: Like
Radarr/Sonarr | Ether" switch and the "What matters most" ladder are
removed. A TRaSH Guides or Profilarr import is a configuration pack (a
quality order, custom formats, scores and a minimum score), and it ranks
the way its authors wrote it.

## The comparer

Releases that pass every filter are sorted by the first key that differs:

| # | Key | From | Notes |
| --- | --- | --- | --- |
| G1 | Only-play fallback tier | Ether rule | An "Only play … otherwise" rule's fallback releases come after its primary ones. |
| G2 | Audio language is acceptable | Ether, viewer | A release that declares only languages nobody asked for comes last. |
| G3 | Fits the viewer's connection | Ether, session | A release the connection can't carry comes after one it can. |
| S1 | Quality: position in the profile's quality order | Radarr/Sonarr `CompareQuality` | Qualities in one group tie. Ether grades the quality first (below). |
| S2 | Revision (Repack/Proper) | `CompareQuality` | Only when the profile scores no Repack/Proper format: Radarr's "Prefer and upgrade". A profile that scores one (TRaSH, and Ether's defaults) gets "Do not prefer", as TRaSH instructs. |
| S3 | Custom format score | `CompareCustomFormatScore` | Sum of every matching format's score on this profile. |
| S4 | Preferred protocol | `CompareProtocol` | The owner's source preference (Usenet or debrid). "Automatic" has none. |
| S5 | Indexer priority | `CompareIndexerPriority` | The indexer's Priority setting, lower first. |
| S6 | Seeders, then peers (both torrents) or age (both Usenet) | `ComparePeersIfTorrent`, `CompareAgeIfUsenet` | Rounded log10 of seeders/peers; Usenet age buckets (< 1 h, ≤ 24 h, ≤ 7 days, then −round(log10 days)). |
| S7 | Size | `CompareSize` | Larger first, in 200 MB steps. Ether has no quality definitions with a preferred size yet, so this is Radarr's no-preferred-size rule. A season pack's size is the whole season's, so it never compares with a single episode's. |
| T1 | Played fine recently | Ether | A release that streamed without trouble breaks a full tie. |
| T2 | Indexer reliability | Ether | Learned grab success rate of the indexer. |
| T3 | Single episode over season pack | Ether | Only when nothing above tells them apart. |
| T4 | Starts fastest | Ether routing | Among equivalent releases, a cached debrid copy or the source with the faster measured startup goes first (`routing.go`), packs included. Quality is never traded for speed. |

S1–S7 follow `DownloadDecisionComparer` in Radarr (`develop`): quality,
custom format score, protocol, indexer priority, indexer flags, peers if
torrent, age if Usenet, size. Sonarr inserts season pack, episode count and
episode number after protocol and has no indexer flags. Ether leaves
Sonarr's episode keys out: Sonarr prefers packs because it downloads whole
seasons, and Ether streams one episode, so a pack competes like any release
(see Decisions). Ether has no indexer flags (Radarr compares them only with
"Prefer indexer flags" on) and no delay profiles; the owner's source
preference stands in for the delay profile's preferred protocol. Concepts
only: no Radarr/Sonarr code is copied.

### Grading quality

A quality name is a claim. Ether grades it before S1, for every profile:

- A release whose average bitrate is below the floor for its resolution and
  source (a "2160p" Blu-ray encode at 5 Mbps) is graded one resolution
  lower.
- A presumed upscale (named as one, or a 2160p file of a title whose only
  disc release is 1080p) is graded one resolution lower.
- A graded release keeps no HDR or Dolby Vision credit from Ether's
  built-in formats.

Radarr does the same kind of correction from media info after import;
Ether has to do it from the search result.

### Minimum score and "upgrade until"

The **minimum custom format score** works as in Radarr/Sonarr: a release
whose score is strictly below it is not played. It counts the custom
formats you created or imported. Ether's built-in formats, Prefer rules and
plugin points rank releases but don't count toward the minimum, so a
TRaSH minimum of 0 means what its authors meant, and a device that needs
an audio transcode can't push an otherwise wanted release below it.

**Upgrade until** (cutoff quality and cutoff score) and the minimum upgrade
increment are about replacing a file you already keep. A stream has no
file to upgrade, so they don't apply and imports don't carry them.

## Filters, gates and tie-breaks

"Stream now" decides where each piece of context goes. A **filter** removes
a release that can't or mustn't play here. A **gate** orders before quality
because getting it wrong ruins this viewing whatever the quality. A
**format** scores a preference that should trade off against other
preferences. A **tie-break** only decides between otherwise equal releases.

| Context | Placement | Why |
| --- | --- | --- |
| Device can't decode the video codec, or Dolby Vision without an HDR10 base on a non-DV device | Filter (unchanged) | It doesn't play. |
| Above the device's or profile's resolution, outside the size range, CAM when not allowed, outside an imported profile's allowed qualities | Filter (unchanged) | The owner's limits. |
| Never-play and Only-play rules | Filter (unchanged) | The owner's limits. |
| Below the minimum custom format score | Filter | Radarr/Sonarr rule. |
| Recently failed health check, marked bad, blocked, already tried | Filter (unchanged) | Not available now. |
| Unavailable when checked (expired articles, uncached torrent in cached-only mode) | Filter at validation | Ranked validation checks candidates in order and plays the first that is available, so an unavailable release never reaches the viewer. Ranking ahead of quality by *known* availability would make every unchecked 4K lose to a cached 1080p. |
| Would need a video transcode (legacy codec), audio transcode, or HDR tone mapping | Streaming format (penalty) | It still starts and plays. A gate would drop a 4K film to 1080p because the TV can't take TrueHD. Penalties keep today's behaviour: quality first, then the release that plays directly. |
| HDR and Dolby Vision | Built-in format that counts only where the device and profile show it | On an SDR device HDR is a cost, not a gain. |
| Audio language acceptable to the viewer | Gate G2 | A film in a language the viewer doesn't speak is unwatchable; quality can't make up for it. |
| Declares the viewer's language on a foreign title (an English or MULTi release of a Spanish film for an English viewer) | Streaming format | A preference: many viewers want the original with subtitles, so it can't override resolution. |
| Fits the connection | Gate G3 | A release the connection can't carry stalls or forces a transcode down. |
| Series year differs but names no other show | Streaming format (penalty) | Probably right, so it stays playable, but a correctly labelled release of the same quality goes first. |
| Usenet post older than the provider's retention | Streaming format (penalty) | Likely expired; validation would catch it, at a cost in start time. |
| Played fine recently | Tie-break T1 | Evidence about the past, not about this release's quality. |
| Known cached on debrid, measured startup | Tie-break T3 | Starts faster; never worth a lower quality. |

## Built-in custom formats

Ether ships these formats. They are evaluated from Ether's parsed release
and the session, and appear in every profile's format list with a score,
like the formats you import. Scores are per profile; 0 means off.

Release formats (preferences; the defaults below are the 4K profile's):

| Format | Matches | 4K default |
| --- | --- | --- |
| Remux, Blu-ray, WEB-DL, WEBRip, HDTV, DVD | The release type | 8000, 6000, 4000, 2000, 3, 3 |
| Dolby Vision | Dolby Vision the device and profile show | 500 |
| HDR | HDR10, Dolby Vision or a UHD remux the device and profile show | 500 |
| HDR10 | An HDR10 layer the device and profile show | 8 |
| HEVC, AV1, H.264 (also VP9, VC-1, MPEG-2, XviD) | The video codec, when the device decodes it | 18, 12, 6 |
| TrueHD, E-AC-3, DTS-HD, AC-3, AAC (also DTS, FLAC, Opus, MP3) | The audio format | 20, 16, 12, 8, 4 |
| Atmos | Dolby Atmos | 3 |
| Repack/Proper | A Repack or Proper | 6 |
| Low bitrate | Below the bitrate floor for its resolution | −60 |
| Preferred language, Preferred language (other region), Not a preferred language, Preferred subtitles | The profile's Languages | 5, 2, −25, 2 when Languages are set |

Streaming formats (context Ether knows and Radarr doesn't):

| Format | Matches | Default |
| --- | --- | --- |
| Viewer's language | Foreign title; release declares the viewer's audio language or MULTi | 20000 |
| Series year differs | Accepted although the series year differs | −10000 |
| Video transcode | The device can't decode a legacy codec | −20 |
| Audio plays directly | The device plays the audio format | 10 |
| Audio transcode | The device can't play the audio format | −15 |
| Atmos passthrough | Atmos in a format the device plays | 5 |
| Tone mapping | HDR10 on a device or profile without HDR | −15 |
| Past retention | Usenet post older than provider retention | −20 |

A profile's device formats are scored when Ether creates or imports it,
because device fit is Ether's job; at these sizes they only separate
releases a pack scores alike. Imports get Series year differs too, but not
Viewer's language: language preference is the pack's to express. Series
year differs only matches episodes, so only TV profiles score it and the
console lists it only on them.

Prefer rules are formats too: each Prefer rule has a score (a new one
starts above everything else the profile scores). A plugin ranking step
adds the number the plugin returns for a release to its score.

The minimum score counts only the owner's and imported formats, not
built-ins, Prefer rules or plugin points.

## Default profiles

The shipped 4K, 1080p and 720p profiles are standard profiles: a quality
order with one group per resolution, highest first, inside the profile's
resolution range, and the built-in format scores above. Grouping by
resolution and scoring release type as a format is how TRaSH's merged
"WEB 1080p" and SQP profiles work. It also lets the viewer's language rank
above release type within a resolution, as it does today.

Scores are strictly lexicographic: each level is worth more than all the
levels below it can add up to, rounded up to 1, 2 or 5 × 10ⁿ. In the 4K
profile everything below HDR adds up to at most 203 and everything below
release type to 1203. So WEB-DL (4000) always beats WEBRip (2000), and HDR
(500) only decides between releases of the same type. The 1080p and 720p
profiles use the same shape at smaller weights (release type 1000 a step,
Viewer's language 10000, Series year differs −5000).

## Upgrading

Profiles are not converted. When an install upgrades from a release before
this model, any stored profile that still carries an old ranking field
(`ranking`, `rankingConfig`, `evaluationMode`, `preferredSources`,
`preferredCodecs`, `preferredAudio`, `preferLargest`) resets every quality
profile to the shipped defaults, and the resolver's global
`customFormatScores` and `minCustomFormatScore` are dropped. The install
then ranks exactly as a fresh one does. Client rules, the At home and Away
defaults and every device, bandwidth and availability gate are kept; a
default that named a removed profile falls back to the media's first
profile. The same reset runs on the cluster's shared settings, so every
node reads the same profiles. The release notes say so.

### Proof

`internal/resolver/ranking_corpus_test.go` ranks a deterministic corpus:
four titles (an English film, a Spanish film, an animated film, an
episode, a quarter of whose releases are torrent season packs), 160
release pools of 4 to 14 releases, and four devices (Apple
TV 4K with Dolby Vision, an SDR 4K TV, a 1080p phone, a remote browser at
8 Mbps). It does this for the shipped 4K, 1080p and 720p profiles, which
reach the ranker through `config.Open` as on a fresh install. The
golden (`testdata/ranking_corpus_golden.json`) records the full order, including
closest matches below the profile minimum. The test fails on any accepted-set
or order change. Regenerate a reviewed change with:

```sh
go test ./internal/resolver -run '^TestRankingCorpusMatchesGolden$' -update-ranking-corpus
```

For v0.3.11 the full corpus was compared against the parent of the soft-minimum
change (`a21b0b6b9`). All 1,920 cases preserve every previously accepted release,
in exactly its previous order, as a strict prefix. The new ranking appends 2,060
closest matches across 951 cases; it removes no releases. The historical golden
predated the Radarr/Sonarr comparer and already differed from that parent in 81
cases. Those existing comparer differences are included in this refreshed
snapshot; they were not caused by soft minimums.

## Decisions

Owner decisions on this design (v0.3.6):

1. **Gates before Prefer rules:** approved as built (G1–G3 rank before
   everything, Prefer rules are scored formats).
2. **Season packs first for episodes:** rejected. Sonarr's season-pack,
   episode-count and first-episode keys are removed. Packs and
   single-episode releases compete on quality, custom format score and the
   later keys like any release; routing evidence (measured startup, cached
   on debrid) decides between equivalent ones; with no evidence the single
   episode wins only the final tie-break T3. There is no pack format or
   penalty, so streaming episodes out of Usenet season packs (#378) can
   pick a pack when it is the better release.
3. **Grading upscales and starved encodes on imported TRaSH profiles:**
   approved.
4. **Device formats at their default scores on imports:** approved.
5. **No preferred size yet; larger wins at the last standard key:**
   approved.
6. **A release with no resolution in its name ranks last:** approved.
7. **Plugin `defaultPosition`:** removed from the manifest schema and
   types, the SDKs, examples and validation. There is no ladder to place a
   step in; a plugin step adds its points to the custom format score.
8. **Existing profiles:** a clean cut, no conversion. Upgrades reset every
   quality profile to the shipped defaults (see "Upgrading"), so an
   upgraded install is exactly a fresh install of this design. Device,
   bandwidth and availability gates are untouched.

## Coming from Radarr, Sonarr or TRaSH

- **Quality profiles** are the same idea: an ordered list of qualities
  (with groups), custom formats with scores, a minimum score. Import yours
  from TRaSH Guides, Profilarr, or paste the JSON.
- **Ordering** is Radarr/Sonarr's: quality, then custom format score, then
  protocol, indexer priority, seeders or age, size.
- **What Ether adds:** it won't play something the device can't decode,
  that the connection can't carry, or in a language the viewer doesn't
  accept; it grades starved encodes and upscales one resolution down; and a
  few streaming formats (audio transcode, tone mapping, …) separate
  releases your pack scores alike. Set any of them to 0.
- **What doesn't apply:** upgrade until, cutoff score and the upgrade
  increment (nothing is kept to upgrade), delay profiles and indexer flags.
- **Propers and repacks:** a profile that scores a Repack/Proper format
  (TRaSH's do) works as "Do not prefer"; one that doesn't works as "Prefer
  and upgrade".
- **Language:** a profile's Language works as in Radarr. Original (the
  default) accepts each title's original language alongside the profile's
  Languages and the viewer's own audio languages, and languages are not a
  hard filter except for releases dubbed into a language nobody asked
  for. Any accepts every language. A specific language rejects releases
  whose audio isn't in it — multi/dual-audio releases pass, and a release
  that declares no audio counts as the title's original language.

## Changing quality groups through MCP

`quality_profiles` reads each profile's media, quality groups, custom format
scores, language settings and limits. `quality_order_set` proposes replacing
one profile's groups; the owner approves the change in the console. Pass
`media` when movie and TV profiles share a name. Other profile settings stay
as configured.

For a viewer who wants Spanish 1080p ahead of English 4K, read the profile
first, then put the relevant 1080p and 2160p qualities in one group. Keep
other groups in their existing order. The existing `ether:viewer-language`
score can then decide between those resolutions. For example, a shared WEB
group is:

```json
{
  "profile": "4K",
  "media": "tv",
  "qualityOrder": [
    {"name": "HD and UHD WEB", "qualities": ["webdl-2160", "webrip-2160", "webdl-1080", "webrip-1080"]}
  ]
}
```

The argument replaces the entire order: include any other groups you want
to retain. Unlisted qualities rank below listed ones; an empty order makes
all qualities equal. Grouping changes the priority of every scored format
within that group, not only language. Existing Proper/Repack revision
preferences still apply before scores (S2 above). A profile's language rules, allowed
qualities, size and resolution limits, and the device and availability
checks still apply. A language score alone cannot outrank a higher group.

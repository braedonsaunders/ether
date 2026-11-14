# Configured Plex version slots (v0.3.12)

A cold browse response must expose the same menu identities as a warm detail,
playQueue and playback decision. The previous menu was release-dependent:
hubs showed Auto, discovery added releases on detail, and LG/VIDAA bypassed
versions altogether. Plex's TV settings can fail before asking for a decision
when a previously absent MediaVersion control becomes necessary.

## Contract

`resolver.configuredSlots` is the single menu policy. It reads owner settings
and the effective profile/device ceiling, without searching or resolving a
release. Auto is first. The default mode reserves the configured same-quality
alternatives and three distinct tier/cut choices; the existing seven-entry
cap applies. The per-resolution and per-source modes reserve their tier/source
choices in rounds so one busy resolution cannot consume the whole menu.
Unknown availability leaves a stable placeholder, rather than removing a row.

A slot's selector is its identity. Numeric Media/Part IDs bind title plus
selector, independently of the selected release. Cached releases may update
labels (resolution, source, group) and estimated tracks; the selected source's
probe updates its actual tracks. These changes do not alter slot count,
order or IDs. No product-name matching chooses a different menu policy.

Plex's configured-slot capability projects the menu on hubs, detail and queue
responses. A decision retains that list and replaces only the selected slot
with its decision projection. Session monitoring still describes the admitted
source. Excluding Part from a queue continues to leave `partCount=1` per slot.
Lower output quality does not rebuild the menu used to validate mediaIndex.

This deliberately changes decision responses from the captured PMS's selected
Media-only projection for the configured-menu path. Device verification must
check MDE clients with the full list; this is not a claim of PMS byte parity.

## Admission and safety

Only playback admission interprets a logical selector. It reconstructs the
currently permitted plan and selects a candidate after normal discovery and
policy filtering. Higher tiers require the owner opt-in and device support.
Missing/unhealthy slots use the existing explicit-choice fallback; they do
not authorize a source outside policy. `Resolution.VersionSelector` records
only a successfully fulfilled selector, so playback can distinguish logical
success from fallback when comparing a concrete source ID with the request.

The stable selector is not an authorization token. Existing viewer, title,
source and playback admission checks still apply. The slot plan is bounded
by seven entries and is built only for requested titles. Hub rendering never
runs an indexer search or full resolution. The numeric-ID registry now reads
persisted mappings by indexed key instead of loading the whole registry on
first access; its existing collision handling and retention remain in use.

## Verification

Focused offline checks cover:

- Configured counts and IDs before/after discovery, per-resolution/source
  modes, real release labels, explicit slot selection, higher-tier opt-in,
  device ceilings, and unavailable-slot fallback.
- Hub → cold detail → discovery → playQueue → explicit decision → cache
  expiry → lower-quality decision → stop/resume decision for Plex Web,
  Android TV, LG, VIDAA and Samsung, with identical Media/Part identities.
- Playback preserving a successful logical choice over remembered Auto,
  then reopening that choice after stop without marking it stale.
- Persisted numeric IDs and collision checks using point lookups.

`CGO_ENABLED=0 go build ./...`, focused tests and vet on changed packages
pass. The coordinator owns the full release gate. Real-device validation is
pending; the owner TV became unreachable during investigation. Before shipping
v0.3.12, test the picker, Original quality, lower output quality, seeking,
resume, subtitles, unavailable slots and real-release labels on Web, Android
TV and the LG/VIDAA/Samsung TV app family. The original issue-483 reporter
sessions got as far as DASH manifests, so their separate init/segment rejection
also still needs a real retest; this change does not prove that issue resolved.

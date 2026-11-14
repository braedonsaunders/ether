# Roku / Shield follow-up on v0.3.10

The 14 November report does not include the image IDs, the failing `/items`
query, or the Shield app version. Those failures cannot all be attributed to
one root cause from the redacted paths. These are confirmed compatibility gaps
found while tracing the official clients, with device checks still required.

## Android TV

The previous fix matched only `Jellyfin for Android TV`. Official 0.18.11 uses
`Android TV`; 0.19.7 uses `Jellyfin Android TV`; 0.20.0 uses `Jellyfin for Android
TV`. All use the Kotlin SDK's changing device identity during sign-in. The
0.19 and 0.20 debug builds append ` (debug)`. The same bearer-token exception
now covers these exact official names, retaining the stored device for all
subsequent scoped capabilities. Quick Connect replay uses the same names and
still checks the surface, secret, expiry, current user, and live grant.

The password body is `Username` / `Pw`, with no case conversion, trimming, or
password decoding. Quick Connect still follows Initiate → Connect (poll) →
AuthenticateWithQuickConnect. There is no evidence for accepting legacy hashed
`Password` fields or changing local username semantics.

Sources reviewed, no code copied:

- [0.18.11 AppModule](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.18.11/app/src/main/java/org/jellyfin/androidtv/di/AppModule.kt)
- [0.19.7 AppModule](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.19.7/app/src/main/java/org/jellyfin/androidtv/di/AppModule.kt)
- [0.20.0 AuthenticationRepository](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.20.0/app/src/main/java/org/jellyfin/androidtv/auth/repository/AuthenticationRepository.kt)
- [0.20.0 SessionRepository](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.20.0/app/src/main/java/org/jellyfin/androidtv/auth/repository/SessionRepository.kt)

## Roku images and browsing

Roku 3.2.4 `PosterImage` calls `/Items/{id}/Images`, then constructs an image
URL without `tag`. `ImageData` assigns Width and Height to integer SceneGraph
fields. Ether omitted those properties from ImageInfo, even when known; they
now appear (zero when unknown). This is a DTO mismatch, not proof of the exit.

Authenticated tagless images now use cache-only ItemArtwork, rather than
full item enrichment, and resolve the edition handles and authorized person
IDs that Ether itself emits. The image list also resolves edition handles.
Local image denials use the JSON ProblemDetails shape. Anonymous artwork keeps
its existing intersection-of-viewers policy; restricted images still require
a credential or capability. A 404 from that policy is intentional.

Unfiltered recursive TV lists with title sorting, mixed root/child types, or
non-favorite predicates could still expand all series. They now merge bounded
stored SQL pages with parent visibility checks. Single-type pages pass offset
and limit directly to SQL. No provider refresh or season hydration occurs on
this path. Existing favorite and played optimizations remain first. Custom
orders, positive-favorite fallback, anchor queries, and mixed windows above
1000 retain their existing paths; this does not claim to eliminate every
pre-existing scan in the experience service.

- [Roku 3.2.4 Image API](https://github.com/jellyfin/jellyfin-roku/blob/3.2.4/source/api/Image.bs)
- [Roku 3.2.4 ImageData](https://github.com/jellyfin/jellyfin-roku/blob/3.2.4/components/data/ImageData.xml)
- [Roku 3.2.4 metadata and playback requests](https://github.com/jellyfin/jellyfin-roku/blob/3.2.4/source/api/Items.bs)

## Live logs and device verification

The process text/file handler previously captured a fixed slog.Level at
startup. It now shares a slog.LevelVar with the app, subscribed to config
updates for every node role. Existing child loggers see the new threshold
immediately. A startup flag remains effective until a configuration edit.
Authentication failures also emit fixed stage/reason values at Debug, never
submitted names, passwords, device identifiers, codes, or polling secrets.

On Windows, set Debug without restarting and export one attempt; confirm DEBUG
request lines exist, then restore Info. On Shield record the app version, test
password sign-in, restart/token restore, Quick Connect approval and redemption,
and a repeated redemption. On Roku play a film and resume an episode. Capture
the sanitized `/items` query, image route/status, and progression through
PlaybackInfo → stream → Sessions/Playing. If an image still 404s, identify its
ID *class* (title, edition, person, view) and whether the request carried a
tag/credential, without publishing the value. A generic aggregate 401 summary
cannot distinguish bad password, device binding, expired polling, or a later
request failure.

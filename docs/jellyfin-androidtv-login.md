# Android TV sign-in investigation

See [the v0.3.11 follow-up](investigations/421-roku-shield-0311.md) for
the additional official client names missed by this initial fix.

Reviewed latest release v0.20.0 and its pinned jellyfin-sdk-kotlin v1.8.12,
read-only. No upstream implementation was copied.

## Wire contract

- Credentials: POST `/Users/AuthenticateByName`, JSON `Username` and `Pw`
  (plaintext). No `Password`, and no `/Users/{id}/Authenticate` call.
- Username and password come directly from the text fields, without trimming
  or case folding. The public picker fetches `/Users/Public` and passes the
  selected user's **name** to the same credential flow, not an ID endpoint.
- SDK OkHttp sends `Authorization: MediaBrowser Client="Jellyfin+for+Android+TV",
  Version="0.20.0", DeviceId="…", Device="…"`, adding `Token` after login.
  It does not use X-Emby-Authorization. Header values are form encoded:
  spaces become `+`. Ether already accepts both headers and decodes these.
- Base device ID is Android's ANDROID_ID (normally 16 hexadecimal characters).
  Credential login uses SHA-1(base ID + "+" + entered username), 40 lowercase
  hexadecimal characters. SessionRepository switches immediately to
  SHA-1(base ID + "+" + dashed user UUID), then calls GET `/Users/Me`.
- Quick Connect checks `/QuickConnect/Enabled`, initiates and polls with a
  SHA-1 ID derived from a random UUID, redeems via
  `/Users/AuthenticateWithQuickConnect` with JSON `Secret` using the base ID,
  then switches to the user-UUID hash as above.

## Ether divergence and fix

The credential body, endpoint, casing, header encoding and device length are
already compatible on the requested integration base. Identity's local password
lookup is exact and neither this client nor this fix changes that behavior.
The earlier long-ID / plus-decoding fix does not address rotating SHA-1 IDs.

The password request succeeds, but strict grant-device validation rejects the
immediate `/Users/Me`, so Android TV cannot establish its session. The old
Android native-player suffix accommodation cannot correlate two independent
hashes. Quick Connect retries also rejected the base ID against the original
random-user hash.

For grants issued to the release client's exact name, Jellyfin now accepts the
valid bearer token across these ID changes. It resolves the original stored
device and uses that device for downstream capability scopes; it does not
rebind or write on requests. This deliberately means a holder of such a token
can use another claimed device ID with the same client name: the hashes cannot
prove installation continuity. Other clients and Emby keep strict binding.
Quick Connect replay permits the same client's changed ID only on Jellyfin,
while retaining secret, expiry, revocation, viewer and surface checks.

Validation: `TestAndroidTVSignIn` replays the credential request followed by
the UUID-hash `/Users/Me`, and rejects a foreign client or invalid token.
The focused identity Quick Connect test, vet of both touched packages and
`go build ./...` passed. Real Shield credential/picker login, restart with
stored token, Quick Connect including a retried redemption, and iPhone login
still require device verification by the integration coordinator.

## Upstream references

- [Latest release](https://github.com/jellyfin/jellyfin-androidtv/releases/tag/v0.20.0)
- [AuthenticationRepository](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.20.0/app/src/main/java/org/jellyfin/androidtv/auth/repository/AuthenticationRepository.kt)
- [SessionRepository](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.20.0/app/src/main/java/org/jellyfin/androidtv/auth/repository/SessionRepository.kt)
- [DeviceInfoExtensions](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.20.0/app/src/main/java/org/jellyfin/androidtv/util/sdk/DeviceInfoExtensions.kt)
- [Quick Connect flow](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.20.0/app/src/main/java/org/jellyfin/androidtv/ui/startup/UserLoginViewModel.kt)
- [Public picker](https://github.com/jellyfin/jellyfin-androidtv/blob/v0.20.0/app/src/main/java/org/jellyfin/androidtv/ui/startup/fragment/ServerFragment.kt)
- [SDK wire body](https://github.com/jellyfin/jellyfin-sdk-kotlin/blob/v1.8.12/jellyfin-model/src/commonMain/kotlin-generated/org/jellyfin/sdk/model/api/AuthenticateUserByName.kt)
- [SDK header transport](https://github.com/jellyfin/jellyfin-sdk-kotlin/blob/v1.8.12/jellyfin-api-okhttp/src/jvmMain/kotlin/org/jellyfin/sdk/api/okhttp/OkHttpClient.kt)

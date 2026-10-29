# Simkl integration

Ether uses Simkl AUTH V2. The owner registers an application and supplies its
client ID; server-app registrations also require a client secret. Device/PIN
authorization requests `media:read media:write`. The detailed device endpoint
specifies a device code grant without PKCE fields, while the authentication
overview says both flows require PKCE. Ether follows the endpoint-specific
schema rather than inventing device parameters:

- [Authentication overview](https://api.simkl.org/authentication)
- [V2 device flow](https://api.simkl.org/api-reference/oauth2-device)
- [V2 token endpoint](https://api.simkl.org/api-reference/simkl/oauth2-token)
- [Token refresh and revocation](https://api.simkl.org/api-reference/oauth2-tokens)
- [History sync](https://api.simkl.org/guides/sync)
- [History writes](https://api.simkl.org/api-reference/simkl/add-to-history)
- [Rate limits](https://api.simkl.org/resources/rate-limits)
- [Anime mapping](https://api.simkl.org/guides/anime)
- [Redirect ID resolution](https://api.simkl.org/api-reference/simkl/redirect)

Imports run on demand and read activities before fetching changed history.
The initial episode pull uses `include_all_episodes=original`, avoiding
synthetic timestamps but omitting episodes that Simkl represents only as a
bulk-completed show. Anime history is skipped because its numbering may not
match TMDB. Exports resolve television type before writing and skip anime
rather than guessing episode mappings. Unwatches and deletions are never
propagated. Simkl's normal history endpoint does not create distinct rewatch
sessions without its separate PRO/VIP feature, which Ether does not enable.

A viewer's pending device flow and tokens are sealed and bound to that viewer
and the registered client ID. Shared leases serialize refresh and background
export across nodes; a successful disconnect fences the grant and removes
queued work. Successful remote delivery can be retried after an interrupted
response; normal Simkl history writes are idempotent without rewatch mode.
The owner should complete credential sealing before connecting a Simkl account.

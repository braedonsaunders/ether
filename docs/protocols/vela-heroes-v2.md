# Vela featured band v2

The [example bootstrap excerpt](vela-heroes-v2.example.json) illustrates six
slots with synthetic titles and non-replayable receipt/image placeholders. It
is for the vela-test client integration, not a captured server response. Obtain
real capabilities from the paired server's authenticated `/vela/v0/bootstrap`.
No account or device credential belongs in the example.

The first eligible personal action follows the existing ranked Home order;
owner-pinned rows take precedence with their honest producer reason. Remaining
slots alternate personal, trending, personal, recent release/addition and
trending, falling back to the next eligible candidate when a category runs out.
Cards keep their original item receipt and preview opt-in. Episodes display
the authorized episode metadata and target that exact episode. Series use
`details`; playable movies and episodes use `play` or persisted-position
`resume`. No title-pin source exists, so production does not emit `featured`.
Labels are omitted when the source provides none.

Only real signed backdrop sources qualify. Known dimensions must be landscape;
unmeasured sources retain 0/0. No pixel fetch, trailer discovery, provider call,
or ranker invocation is added by band composition. Heroes and shelves dedupe
series families, including continuation pages, without changing issued offsets.

Bootstrap and `/vela/v0/rows` request 20 entries per shelf through the row-view
pager. Their `limit` still counts shelves. `/vela/v0/rows/{id}` defaults to 20
entries and accepts an item `limit` capped at 50; missing, invalid or nonpositive
values use the default. Hero exclusions can shorten the displayed shelf without
changing its source page size or cursor offset. Metadata and artwork remain
batched per row; no extra fill queries run to replace excluded heroes.

When Home lacks six eligible cards, an internal Home-only request fills the
open slots through the existing policy, owner layout and receipt pipeline.
Persisted weekly charts supply trending candidates without refresh. Recent
release fallback uses the `catalog_hero_recent` partial ordered index. Art,
viewer rules, content rating, availability and exclusions apply before LIMIT;
only the returned six-or-fewer titles have their local metadata batched in.
Explicit owner source rules apply before queries, and shelf sparsity/default
shelf slots do not discard a one-card band fill. Fallback requests bypass the
normal snapshot cache and cannot replace Home manifests.

For the paired release, verify on real devices:

- A returning viewer gets the expected resume or next episode, with the exact
  saved offset and episode target; a new viewer gets honest discovery reasons.
- A normal library produces five or six distinct backdrop-bearing heroes;
  artless, restricted and excluded titles never fill the band.
- Owner-hidden and scheduled-off sources stay hidden; pinned source order and
  preview opt-in survive. Unknown image dimensions display at natural aspect.
- Hero titles/series do not recur on shelves or later continuation pages.
- A cold or stale trending chart fills from recent releases without a provider
  refresh. Preview starts/stops remain isolated from feature playback/history.

The coordinator runs the combined release gate and captures the real vela-test
bootstrap after merging; this worker does not deploy or touch the lab.

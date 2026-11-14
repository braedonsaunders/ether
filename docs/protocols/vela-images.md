# Vela v0 image sizes

Image capabilities retain the existing viewer, device, grant-family and current
artwork-policy checks. Sizing never grants access to another source.

`GET /vela/v0/images/{capability}` uses `w` and `h` as the only dimension query
parameters. Both are non-negative integers. A positive dimension is an upper
bound; Ether fits inside the requested box, keeps the source aspect ratio, and
never upscales. Bounds above 4000 are clamped to 4000. Zero means unconstrained.
Negative, malformed or repeated dimensions, and aliases such as `width` or
`height`, return 400 rather than silently serving a different size.

With no dimensions, or `w=0`, Ether serves the source at its normal resolution.
Backdrops and hero artwork do not implicitly become 500-pixel thumbnails.

An `ArtworkRef.sizes` entry with positive width and height describes actual
source-clamped output pixels; its URL includes matching explicit `w` and `h`.
The descriptor uses cached source metadata and never fetches image bytes while
building a catalogue page. When source dimensions are unmeasured, both are zero
and the URL includes `w=0`: the client can display it and learn natural dimensions
from the image without guessing or injecting a thumbnail width.

Episode title artwork contains the episode's own `kind:"still"` image when
available. Otherwise it contains a `kind:"backdrop"` fallback, then a
`kind:"poster"` fallback if no backdrop exists; the original kind is preserved.
No artwork is returned if none is available. Episode pages use the same bounded
batch signing and viewer/device-scoped capabilities as other title pages.

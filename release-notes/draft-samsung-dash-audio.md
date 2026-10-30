# Streaming audio targets

Ether now respects the audio formats a Plex app advertises for its chosen streaming protocol. A Samsung client requesting AAC over DASH previously received copied E-AC3 audio because Ether used the TV's general file-decoding capabilities for segmented output too. Compatible H.264 video remains copied; only the incompatible audio is converted. Direct play and explicitly supported surround formats are preserved.

Verified offline with the reported Samsung DU7100 / Plex 5.94.1 profile and a synthetic H.264 + E-AC3 file: authenticated decision, DASH manifest, video/audio initialization and first media segments, and decoding of both representations. The reporter's physical TV has not yet retested this change; do not claim the whole device report resolved until that result arrives.

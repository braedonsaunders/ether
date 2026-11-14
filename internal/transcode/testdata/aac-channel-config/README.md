Tiny silent AAC-LC configuration-7 fixtures, generated locally with FFmpeg:

```sh
ffmpeg -f lavfi -i 'color=c=black:s=32x32:r=24' -f lavfi -i 'anullsrc=r=48000:cl=7.1' -t 0.15 -c:v libx264 -pix_fmt yuv420p -c:a aac -threads 1 aac71.mkv
ffmpeg -i aac71.mkv -c copy aac71.mp4
# The installed encoder emits config 12. Change only the ASC config bits
# to 7 to reproduce legacy config-7 conventional-7.1 streams.
python3 - <<'PYTHON'
from pathlib import Path
for ext in ('mkv', 'mp4'):
    p = Path('aac71.' + ext)
    b = p.read_bytes()
    old, new = bytes.fromhex('11e056e500'), bytes.fromhex('11b856e500')
    assert b.count(old) == 1
    p.write_bytes(b.replace(old, new))
PYTHON
ffprobe -v error -show_streams -of json aac71.mkv > aac71.mkv.ffprobe.json
ffprobe -v error -show_streams -of json aac71.mp4 > aac71.mp4.ffprobe.json
```

Both recordings report eight channels. FFmpeg defaults to interpreting config 7
as conventional `7.1` for compatibility with incorrectly encoded streams. The
native parser uses the standard's `7.1(wide)` assignment; config 12 is `7.1`.
See FFmpeg's `ff_aac_set_default_channel_config` documentation:
https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/aac/aacdec.c

Configuration 14 is the eight-channel top layout, named `5.1.2(back)` by current
FFmpeg (the legacy macro is `AV_CHANNEL_LAYOUT_7POINT1_TOP_BACK`).

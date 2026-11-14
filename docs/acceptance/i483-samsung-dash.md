# Samsung DASH playback: issue 483

## Evidence and limits

The supplied v0.3.10 captures identify Plex for Samsung 5.94.1 on
UN55TU7000FXZA (`21_KANTSU2E_UHD_BASIC1`) and UN65NU7100
(`18_KANTME_UHD_BASIC`). Both sessions finish `start.mpd` with HTTP 200;
no init/media request appears before stop. The first TV stops after about
3 seconds, the second after about 27–34 seconds. The captured manifest
sizes are respectively 2214 and 1919 bytes.

These bundles **do not contain MPD bodies, profile-extra values, probed stream
codecs, encoder errors or client-side errors**. `omissions.json` explicitly
records these omissions. The selected release names indicate H.264 SDR with
DDP5.1/Atmos, but the resolution trace is marked title-wide, not request-bound.
Consequently we cannot prove the exact MPD or the cause of either TV's rejection
from these bundles. No Samsung-specific PMS capture is present under
`testdata/conform/plex`; the available captured PMS MPD is
`internal/transcode/testdata/dash/pms-start.mpd`.

## Profile handling

`device_profile_facts.go`'s `Samsung Tizen` has only an HLS streaming target
(`mpegts`, H.264, AAC/MP2/MP3/AC3/EAC3), **no default DASH target**. Its HLS
EAC3 support must not be interpreted as an affirmative DASH declaration.
`streamingAudioTarget` filters client profile-extra targets by protocol/context;
`prepare` passes these separate output codecs to Prism admission. The decider
applies them after ruling out direct play. The DASH start path also re-decides
an otherwise direct-play result when its audio is outside the DASH target.

The existing sanitized DU7100 Samsung profile fixture declares DASH MP4,
H.264/HEVC and **AAC only**. The expanded real-FFmpeg test demonstrates:

- That profile converts EAC3 5.1 to AAC stereo (`mp4a.40.2`), even when
  directPlay=1 was originally allowed and the player subsequently requests DASH.
- A profile explicitly adding EAC3 to that target preserves EAC3 5.1 (`ec-3`).
- H.264 remains copied; both init+media outputs decode with FFmpeg.

The DU7100 fixture is not a recovered profile from either issue-483 TV.
There is no evidence that their actual DASH target allowed or denied EAC3.
The new `client.capabilities` event with `stage=output` records the resolved
player profile, output protocol and final target codecs, closing this gap.
No model-name override or new unconditional EAC3 conversion was added.

## Manifest comparison

| Field | Captured PMS | Reproduced Ether |
| --- | --- | --- |
| MPD profiles/type | isoff-live:2011 / static | Same |
| Duration/Period | Full title, PT0S start | Same; 8 seconds in generated fixture |
| availabilityStartTime | Absent | Absent for known-duration VOD |
| maxSegmentDuration | PT10S | PT10S for copied video |
| minBufferTime | PT10S | PT5S, existing intentional startup optimization, unchanged |
| SegmentTemplate | timescale=1, duration=5, startNumber=0 | Same |
| Initialization | relative session/{id}/$RepresentationID$/header | Same structure, scoped stream/read grants appended |
| Media | relative session/{id}/$RepresentationID$/$Number$.m4s | Same structure, scoped grants appended |
| URL XML escaping | XML attributes | `&amp;` between token query parameters; placeholders remain literal |
| MIME | video/mp4, audio/mp4 | Same |
| Video codec | avc1.640028 in captured source | avc1.42c028 from generated baseline H.264 init; follows actual source |
| Audio codec | opus in captured source | mp4a.40.2 for AAC target; ec-3 only for explicitly permitted EAC3 |
| Audio channel descriptor | MPEG scheme, 2 for captured Opus | MPEG scheme, 2 for AAC; **Dolby scheme/F801 for EAC3 5.1 after correction** |

`git log -p v0.3.8..v0.3.10 -- internal/transcode/dash.go internal/transcode/dash_session.go`
shows no renderer change; the only session change adds and then removes Touch.
The one-segment buffer and legacy channel descriptor already existed in v0.3.8.

AC3/EAC3 now use `tag:dolby.com,2014:dash:audio_channel_configuration:2011`
and the layout's four-digit speaker mask, rather than a raw channel count.
MPEG descriptors remain for AAC and other codecs. Unknown Dolby layouts omit
the descriptor instead of inventing a speaker assignment. This is a protocol
correction, **not proof that issue 483 is resolved on either device**.

References:

- [Dolby channel descriptor and speaker-bit table](https://ott.dolby.com/OnDelKits/DDP/Dolby_Digital_Plus_Online_Delivery_Kit_v1.5/Documentation/Playback/SDM/help_files/topics/c_mpd_auchlconfig_playback.html)
- [DASH-IF IOP 4.2, section 9.4.2](https://dashif.org/docs/DASH-IF-IOP-v4.2-clean.htm)
- [Samsung general specifications](https://developer.samsung.com/smarttv/develop/specifications/general-specifications.html)
- [Samsung streaming FAQ: number/time templates](https://developer.samsung.com/smarttv/develop/faq/multimedia-streaming.html)

## Support capture and reporter retest

During an owner-started capture only, MPD responses now retain bounded,
allowlisted protocol facts in `events.jsonl` and `REPORT.md`. URL facts report
relative/absolute structure, template variables, init-header shape and grant
presence; they retain no host, session path or query values. Output capability
facts record the target actually handed to the decider after profile processing.

Ask the reporter, separately on each TV:

1. Install the candidate containing this commit, restart the Samsung Plex app,
   start a support capture, and replay the same East of Eden episode/release at
   Original quality, initially with subtitles off.
2. Let it run for at least 30 seconds, then seek forward and test resume. Report
   picture, sound, time until first frame, and whether any playback error appears.
3. Repeat once with a lower quality selected to force conversion, then test the
   usual subtitle selection if playback succeeds.
4. Export one capture per TV immediately after the attempt. Inspect the output
   protocol/target, MPD codecs and channel descriptor, followed by whether init
   and media segment requests now occur. If it still stops before init, obtain
   the TV's client-side error and a PMS comparison with the same source/profile;
   do not infer a per-model rejection reason from HTTP 200 alone.

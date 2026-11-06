package playbackts

// This first offline increment supplies published-byte identity receipts and a
// bounded transport observation kernel. It is deliberately not wired to recovery.
//
// Supported transport profile: 188-byte packets, one single-packet PAT/PMT,
// exactly one H264 and one MPEG-4 AAC-LC stream, descriptor-free PMT, 48 kHz
// mono/stereo ADTS without CRC and with one raw_data_block, explicit bounded PES
// lengths, strictly increasing observed PES clocks, nonwrapping PCR, no unknown
// PID or duplicate packets. Discontinuity flags are rejected, never cleared.
//
// AAC observations record canonical two-byte ASC, raw-AU SHA256 excluding ADTS,
// and 1024 samples per ADTS frame at 48000 Hz. These do not establish source
// provenance or source-index adjacency. Video observations inspect Annex-B NAL
// envelopes/configuration only: neither PES lengths nor a nominal frame rate
// prove a complete decoded AU or its sample end. No video sample-end duration is
// inferred. Source sample-table/index evidence and video AU-end evidence must be
// supplied by a concrete, approved mux-owned proof before any successful splice
// API is added. No caller-provided booleans/cursors stand in for those proofs.
//
// normalizeCounters is private and tested independently. It copies candidate
// bytes, changes only TS continuity-counter nibbles, reparses the result, and
// checks exact invariance of every other bit. This establishes a transport
// invariant, not that these segments should be joined. There is no exported
// Stitch, Object publication permit, or fake always-unavailable proof producer.
//
// A future proof-gated Stitch must return an uncommitted immutable Object with
// the final byte hash and target instruction. The controller must retain that
// exact final object once, compare actual retained bytes and identity, and CAS
// the same full generation/key/hash. This package grants no publication, encoder
// start, retirement, source-host-loss, external-fencing or client acceptance
// authority. Fresh decoder/preroll and physical playback remain separate checks.

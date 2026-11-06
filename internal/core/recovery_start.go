package core

import "time"

// RecoveryStart is an opt-in instruction for one authorized encoder run, never
// proof of source identity, retirement, mux clocks or successful playback.
// TitlePosition seeks the source; FirstSequence and OutputTimestamp independently
// address the public output. StartOffset must be zero when this is present.
// No implicit retry, seek, fallback or throttle restart may reuse this cursor.
// Actual held bytes require independent consumer validation before publication.
type RecoveryStart struct {
	TitlePosition   time.Duration
	FirstSequence   int64
	OutputTimestamp time.Duration
	SeekEpoch       int64
	// AdmittedDecision is an expected original plan, not an attestation. Trusted
	// composition must verify it against the actual retained admitted plan.
	AdmittedDecision Decision
	Audio            *RecoveryAACCursor
}

// RecoveryAACCursor describes expected consecutive AAC access units. Every
// identity/clock/digest must be verified using the same authenticated source
// lease and retained output. Missing immutable sample/AU indexing is unsupported;
// timestamps or ordinals must never be synthesized from playlist durations.
type RecoveryAACCursor struct {
	SourceRevision string
	StreamIndex    int
	SampleRate     int
	Channels       int
	SamplesPerAU   int
	NextSample     int64
	// Initial AAC-LC profile hashes the canonical two-byte AudioSpecificConfig
	// (48 kHz, mono/stereo, 1024 samples). A known absent-SBR sync extension
	// is normalized away; unknown extensions/configurations are unsupported.
	CodecConfigSHA256 [32]byte
	// Hashes cover canonical raw AAC payload, excluding ADTS transport headers.
	PreviousRawAUSHA256 [32]byte
	NextRawAUSHA256     [32]byte
	SourceNext          RecoveryPacketClock
	OutputNext          RecoveryPacketClock
}

// RecoveryPacketClock is an exact rational clock for the same access unit.
// PTS and DTS are counts in TimeBaseNum/TimeBaseDen seconds, not nanoseconds.
type RecoveryPacketClock struct {
	PTS         int64
	DTS         int64
	TimeBaseNum int64
	TimeBaseDen int64
}

// CloneRecoveryStart detaches all mutable fields before asynchronous admission,
// callbacks or transport hashing. The digest arrays and clock structs are values.
func CloneRecoveryStart(r *RecoveryStart) *RecoveryStart {
	if r == nil {
		return nil
	}
	c := *r
	c.AdmittedDecision.AudioFilters = append([]string(nil), r.AdmittedDecision.AudioFilters...)
	if r.Audio != nil {
		a := *r.Audio
		c.Audio = &a
	}
	return &c
}

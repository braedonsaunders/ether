package frontend

import "context"

// ProgressSample is the exact Silo v2 sample before conversion to duration.
// InstallationID is the server installation identity, not the device ID.
type ProgressSample struct {
	InstallationID  string
	Sequence        uint64
	PositionSeconds float64
	Paused          bool
	// Stop finalizes playback under the same ownership gate as sample ordering.
	Stop bool
}

type ReportOutcome string

const (
	ReportApplied  ReportOutcome = "applied"
	ReportReplayed ReportOutcome = "replayed"
	ReportStale    ReportOutcome = "stale_sample"
	ReportConflict ReportOutcome = "conflict"
)

// SampleReporter reports an idempotent sample and returns its durable outcome.
// It is additive to Sessions.Report, whose existing semantics stay unchanged.
type SampleReporter interface {
	ReportSample(context.Context, SessionAccess, string, ProgressSample) (ReportOutcome, error)
}

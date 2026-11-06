package core

import "context"

// RecoverySourceReader is an optional trusted capability of the SAME claimed
// EncoderInputLease. Identity comes from its authenticated source scope and
// immutable size, never from RecoveryStart or a caller-supplied URL. Absence is
// unsupported. Implementations expose neither credentials nor a raw Reader.
//
// Each synchronous read must register with the lease's existing retirement
// tracker before leaving its admission lock. SealInput rejects new work and
// InputDone must include actual completion of every admitted read; cancellation
// or a deadline alone never proves retirement. Reads share bounded source
// concurrency and input lifetime cancellation. Callers split reads at 64 KiB.
type RecoverySourceReader interface {
	EncoderInputLease
	RecoverySourceIdentity() (revision string, size int64, err error)
	ReadRecoveryAt(ctx context.Context, expectedRevision string, p []byte, off int64) (n int, err error)
}

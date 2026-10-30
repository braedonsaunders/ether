package core

import "context"

// SourceRetirement reports backend responsibility for one exact manager source
// lifetime, including provisional and prior generations. Done is terminal;
// Err is nil only after Done, successful Close of every acquired source and
// settlement of its durable payload cleanup. Pending cleanup keeps Done open;
// a terminal backend Close error never becomes success.
// This does not prove encoder retirement or a durable ownership transfer.
type SourceRetirement interface {
	Done() <-chan struct{}
	Err() error
}

// OwnedSourcePlayback exposes source responsibility even when preparation
// fails or its caller cancels while a shared backend Open is still running.
type OwnedSourcePlayback interface {
	PrepareSurfaceSource(context.Context, *User, RatingKey, ClientCaps, string, bool) (*PlaySession, SourceRetirement, error)
	SourceRetirement(string) (SourceRetirement, bool)
	// RetireSource detaches the exact captured source lifetime and schedules
	// cleanup. Admission failure leaves the session attached. It is idempotent
	// for the same lifetime and cannot detach a replacement.
	RetireSource(string, SourceRetirement) error
}

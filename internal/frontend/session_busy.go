package frontend

import (
	"errors"
	"time"
)

// ErrSessionBusy means bounded broker admission is temporarily exhausted.
var ErrSessionBusy = errors.New("session operation capacity exhausted")

type SessionBusyError struct{ RetryAfter time.Duration }

func (*SessionBusyError) Error() string        { return ErrSessionBusy.Error() }
func (*SessionBusyError) Is(target error) bool { return target == ErrSessionBusy }

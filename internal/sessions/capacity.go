package sessions

import (
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func capacityError(err error) error {
	if errors.Is(err, transcode.ErrTooManyTranscodes) {
		return &frontend.TranscodeCapacityError{RetryAfter: 5 * time.Second, Cause: err}
	}
	return err
}

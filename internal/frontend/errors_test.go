package frontend

import (
	"errors"
	"fmt"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestNotFoundMatchesDomainError(t *testing.T) {
	if !errors.Is(fmt.Errorf("wrapped: %w", core.ErrNotFound), ErrNotFound) {
		t.Fatal("frontend not-found does not match domain error")
	}
}

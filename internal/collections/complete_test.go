package collections_test

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestCompleteListRespectsCancellation(t *testing.T) {
	svc, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.ListComplete(ctx, nil, core.TypeMovie); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled complete list: %v", err)
	}
}

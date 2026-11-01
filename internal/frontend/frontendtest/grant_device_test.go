package frontendtest

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestGrantDeviceFakeIsOptIn(t *testing.T) {
	if _, ok := any(Identity{}).(frontend.GrantDeviceIdentity); ok {
		t.Fatal("ordinary fake unexpectedly advertises grant device support")
	}
	fake := GrantDeviceIdentity{ResolveGrantDeviceFunc: func(_ context.Context, credential, claim string) (*core.Viewer, core.Device, time.Time, error) {
		if credential != "token" || claim != "claim" {
			t.Fatal("arguments changed")
		}
		return &core.Viewer{ID: 3}, core.Device{ID: "stored"}, time.Time{}, nil
	}}
	var identity frontend.GrantDeviceIdentity = fake
	viewer, device, _, err := identity.ResolveGrantDevice(context.Background(), "token", "claim")
	if err != nil || viewer.ID != 3 || device.ID != "stored" {
		t.Fatalf("%+v %+v %v", viewer, device, err)
	}
}

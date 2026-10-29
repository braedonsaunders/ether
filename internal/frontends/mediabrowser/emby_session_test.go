package mediabrowser

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type embyDeviceAllocator struct{ fail bool }

func (a embyDeviceAllocator) Compact(_ context.Context, surface, kind, ref string) (int32, error) {
	if a.fail || surface != "emby" || kind != "device" || ref != "tv" {
		return 0, errors.New("unavailable")
	}
	return 7, nil
}
func (embyDeviceAllocator) Resolve(context.Context, string, string, int32) (string, error) {
	return "", errors.New("unused")
}

func TestEmbySessionUsesDurableDeviceID(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Emby-Device-Id", "tv")
	v := &frontend.Viewer{ID: 1}
	i := &instance{protocol: "emby", host: frontend.Host{IDs: ids.Codecs{Allocator: embyDeviceAllocator{}}}}
	if got := i.sessionDTO(r, v)["InternalDeviceId"]; got != int32(7) {
		t.Fatalf("device ID: %#v", got)
	}
	i.host.IDs.Allocator = embyDeviceAllocator{fail: true}
	if _, ok := i.sessionDTO(r, v)["InternalDeviceId"]; ok {
		t.Fatal("invented device ID after allocator failure")
	}
}

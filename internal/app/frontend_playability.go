package app

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

type estimatePlanner interface {
	Plan(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error)
}

// detailCanPlay asks the broker about authorization without preparing a source.
// A missing trusted peer or an indeterminate broker failure stays unknown.
func detailCanPlay(ctx context.Context, req frontend.ItemRequest, protocol core.Protocol, capabilities frontend.Capabilities, broker estimatePlanner) *bool {
	ip, ok := frontend.ClientIP(ctx)
	if !ok || ip.IsUnspecified() || ip.IsMulticast() || req.Viewer == nil || req.Viewer.ID <= 0 || req.Device.ID == "" || req.Key == 0 || protocol == "" || capabilities == nil || broker == nil {
		return nil
	}
	device := req.Device
	result, err := capabilities.Build(ctx, frontend.CapabilityRequest{Viewer: req.Viewer, Device: caps.DeviceInfo{Product: device.Product, Platform: device.Platform, Version: device.Version, DeviceName: device.DeviceName, ClientID: device.ID}})
	if err != nil {
		return nil
	}
	_, err = broker.Plan(ctx, frontend.PlanRequest{Viewer: req.Viewer, Device: device, Protocol: protocol, Item: req.Key, Caps: result.Client, Intent: frontend.PlayIntent{Estimate: true}})
	if err == nil {
		allowed := true
		return &allowed
	}
	if errors.Is(err, core.ErrUnauthorized) || errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnreleased) {
		allowed := false
		return &allowed
	}
	return nil
}

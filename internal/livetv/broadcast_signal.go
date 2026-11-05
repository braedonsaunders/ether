package livetv

import (
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/onair/families"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/scene"
	"github.com/braedonsaunders/ether/internal/onair/signal"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type liveStaticSource interface {
	LiveStatic(context.Context, core.ClipRequest) bool
}

// Signal is a live generator, not a decoded looping master. State and plate/
// typography are prepared once away from the clock; only Render/blend run per tick.
type signalInputDecoder struct {
	engine  *signal.Engine
	overlay *videoOverlay
	format  transcode.RawFormat
	tick    int64
	audio   []int16
	closed  chan struct{}
	once    sync.Once
}

func (d *signalInputDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	select {
	case <-ctx.Done():
		return transcode.RawFrame{}, ctx.Err()
	case <-d.closed:
		return transcode.RawFrame{}, context.Canceled
	default:
	}
	frame := transcode.RawFrame{Video: make([]byte, d.format.Width*d.format.Height*3/2), PCM: make([]byte, 6400)}
	d.engine.Render(frame.Video, d.audio, d.tick)
	d.tick++
	for i, v := range d.audio {
		binary.LittleEndian.PutUint16(frame.PCM[i*2:], uint16(v))
	}
	d.overlay.blend(frame.Video, d.format, 255)
	return frame, nil
}
func (d *signalInputDecoder) Close() error { d.once.Do(func() { close(d.closed) }); return nil }
func (s *Service) openSignalDecoder(ctx context.Context, request core.ClipRequest, f transcode.RawFormat) (transcode.Decoder, func(), error) {
	if f.FPSNum != 30 || f.FPSDen != 1 {
		return nil, nil, fmt.Errorf("live Signal requires the channel's 30 fps working clock")
	}
	style := strings.TrimPrefix(request.Moment, "screen.")
	isStatic := style == "static"
	var doc scene.Document
	var bindings raster.Bindings
	var ok bool
	if source, exists := s.o.Graphics.(overlaySource); exists {
		doc, bindings, ok = source.Overlay(ctx, request)
	}
	if !ok {
		moment := "screen." + style
		if isStatic {
			moment = "screen.static"
		}
		var err error
		doc, err = families.Get("signal", moment)
		if err != nil {
			doc, err = families.Get("signal", "screen.card")
		}
		if err != nil {
			return nil, nil, err
		}
		bindings = raster.Bindings{Bindings: families.Defaults("signal")}
		bindings.Slots["channel.name"] = request.Bind["channel.name"]
		bindings.Slots["station.id"] = ""
		if request.Bind["screen.stationId"] == "1" {
			bindings.Slots["station.id"] = request.Bind["channel.number"] + " · " + request.Bind["channel.name"]
		}
		switch request.Bind["screen.reason"] {
		case offAirSlate:
			bindings.Slots["screen.line1"] = "Off air"
			if back := request.Bind["screen.back"]; back != "" {
				bindings.Slots["screen.line1"] = "Back at " + back
			}
		case "busy":
			bindings.Slots["screen.line1"] = "Waiting for a free connection"
		default:
			bindings.Slots["screen.line1"] = "Reconnecting…"
		}
	}
	var engine *signal.Engine
	var overlay *videoOverlay
	seed := int64Hash(request.ChannelID + request.LookID)
	if isStatic {
		engine = signal.NewStatic(f.Width, f.Height, seed)
		text := doc
		text.Layers = signalTextLayers(doc.Layers)
		text.Post = nil
		overlay = compactOverlay(raster.Render(text, bindings, doc.Poster, f.Width, f.Height), f)
	} else {
		plate := raster.Render(doc, bindings, doc.Poster, f.Width, f.Height)
		style = normalizePlateStyle(style)
		engine = signal.NewPlate(f.Width, f.Height, style, plate, image.Image(nil), seed)
	}
	return &signalInputDecoder{engine: engine, overlay: overlay, format: f, tick: s.o.Now().Unix() * 30, audio: make([]int16, 3200), closed: make(chan struct{})}, nil, nil
}
func normalizePlateStyle(style string) string {
	switch style {
	case "bars", "testcard", "brb", "card":
		return style
	}
	return "card"
}
func signalTextLayers(layers []scene.Layer) []scene.Layer {
	var out []scene.Layer
	for _, layer := range layers {
		if layer.Type == "text" {
			out = append(out, layer)
		} else if len(layer.Layers) > 0 {
			layer.Layers = signalTextLayers(layer.Layers)
			if len(layer.Layers) > 0 {
				out = append(out, layer)
			}
		}
	}
	return out
}
func (s *Service) signalPlan(c Channel, reason, back string, f transcode.RawFormat) broadcastPlan {
	request := s.screenRequest(c, reason)
	request.Bind["screen.back"] = back
	id := fmt.Sprintf("signal-%s-%d", c.ID, int64Hash(fmt.Sprint(request)))
	return broadcastPlan{element: "screen", id: id, failed: func(error) time.Time { return s.o.Now().Add(time.Second) }, open: func(ctx context.Context) (transcode.Decoder, func(), error) {
		return s.openSignalDecoder(ctx, request, f)
	}}
}

func (s *Service) screenRequestWithBack(c Channel, reason, back string) core.ClipRequest {
	request := s.screenRequest(c, reason)
	request.Bind["screen.back"] = back
	return request
}

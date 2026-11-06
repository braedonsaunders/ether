package livetv

import (
	"context"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type breakObservation struct {
	input  markers.BreakInput
	points []markers.BreakPoint
}
type breakTapDecoder struct {
	transcode.Decoder
	mu     sync.Mutex
	once   sync.Once
	closed bool
	tap    *markers.BreakTap
	input  markers.BreakInput
	format transcode.RawFormat
	at     time.Duration
	output chan<- breakObservation
}

func (d *breakTapDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	frame, err := d.Decoder.Next(ctx)
	if err != nil {
		return frame, err
	}
	d.mu.Lock()
	if !d.closed {
		span := time.Duration(d.format.FPSDen) * time.Second / time.Duration(d.format.FPSNum)
		d.tap.Frame(d.at, span, frame.Video, frame.PCM, d.format.Width, d.format.Height)
		d.at += span
	}
	d.mu.Unlock()
	return frame, nil
}
func (d *breakTapDecoder) Close() error {
	d.once.Do(func() {
		d.mu.Lock()
		d.closed = true
		points := d.tap.Points()
		d.mu.Unlock()
		if len(points) > 0 {
			select {
			case d.output <- breakObservation{d.input, points}:
			default:
			}
		}
	})
	return d.Decoder.Close()
}
func (s *Service) storeBreakObservation(observed breakObservation) {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if s.o.BreakAnalyzer.ObservePoints(ctx, observed.input, observed.points) == nil {
		merged, err := s.o.BreakAnalyzer.CachedBreakPoints(ctx, observed.input)
		if err != nil {
			return
		}
		observed.points = merged
		// Publishing is deferred until the next background planning pass. The current
		// airing's guide/act slices must not change in response to its decoder frames.
		if len(s.observedBreaks) >= 256 {
			for key := range s.observedBreaks {
				delete(s.observedBreaks, key)
				break
			}
		}
		s.observedBreaks[observed.input.Key] = observed
	}
}

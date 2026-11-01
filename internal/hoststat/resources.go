package hoststat

import (
	"context"
	"sync"
	"time"
)

// Timespan values match the Plex Dashboard bucket sizes: 6 second, 4 hour,
// 3 day, 2 ISO week, 1 month, 0 year. Resources only keeps per-second rows
// on the wire; coarser tiers exist so old samples stay bounded.
const (
	SpanYear  = 0
	SpanMonth = 1
	SpanWeek  = 2
	SpanDay   = 3
	SpanHour  = 4
	SpanSec   = 6
)

var resourceCaps = map[int]int{
	SpanSec:   3600,
	SpanHour:  24 * 14,
	SpanDay:   90,
	SpanWeek:  52,
	SpanMonth: 24,
	SpanYear:  10,
}

var resourceOrder = []int{SpanSec, SpanHour, SpanDay, SpanWeek, SpanMonth, SpanYear}

// Sample is one host/process observation at a bucket start.
type Sample struct {
	At         time.Time
	HostCPU    float64
	ProcessCPU float64
	HostMem    float64
	ProcessMem float64
}

// Resources holds a bounded ring of host samples. Cluster answers come from
// this node only.
type Resources struct {
	now    func() time.Time
	loc    *time.Location
	sample func() (Sample, bool)

	mu    sync.Mutex
	tiers map[int]*resourceTier
}

type resourceTier struct {
	span   int
	points []Sample
}

// Options configure the sampler clock and the sample function (tests).
type Options struct {
	Now    func() time.Time
	Loc    *time.Location
	Sample func() (Sample, bool)
}

// NewResources starts empty. Run samples once a second; tests call Add.
func NewResources(opts Options) *Resources {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Loc == nil {
		opts.Loc = time.Local
	}
	if opts.Sample == nil {
		opts.Sample = hostSample
	}
	r := &Resources{now: opts.Now, loc: opts.Loc, sample: opts.Sample, tiers: map[int]*resourceTier{}}
	for _, span := range resourceOrder {
		r.tiers[span] = &resourceTier{span: span}
	}
	return r
}

// Run records one sample per second until ctx is done.
func (r *Resources) Run(ctx context.Context) {
	if sample, ok := r.sample(); ok {
		r.Add(sample)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if sample, ok := r.sample(); ok {
				r.Add(sample)
			}
		}
	}
}

// Add records a sample into the per-second tier and rolls older points up.
func (r *Resources) Add(s Sample) {
	if s.At.IsZero() {
		s.At = r.now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addLocked(SpanSec, s)
}

func (r *Resources) addLocked(span int, s Sample) {
	s.At = BucketStart(s.At, span, r.loc)
	tier := r.tiers[span]
	if tier == nil {
		return
	}
	if n := len(tier.points); n > 0 && tier.points[n-1].At.Equal(s.At) {
		tier.points[n-1] = mergeResource(tier.points[n-1], s)
		return
	}
	tier.points = append(tier.points, s)
	capN := resourceCaps[span]
	for len(tier.points) > capN {
		old := tier.points[0]
		tier.points = tier.points[1:]
		if next, ok := nextResourceSpan(span); ok {
			r.addLocked(next, old)
		}
	}
}

func mergeResource(a, b Sample) Sample {
	return Sample{
		At:         a.At,
		HostCPU:    (a.HostCPU + b.HostCPU) / 2,
		ProcessCPU: (a.ProcessCPU + b.ProcessCPU) / 2,
		HostMem:    (a.HostMem + b.HostMem) / 2,
		ProcessMem: (a.ProcessMem + b.ProcessMem) / 2,
	}
}

func nextResourceSpan(span int) (int, bool) {
	for i, s := range resourceOrder {
		if s == span && i+1 < len(resourceOrder) {
			return resourceOrder[i+1], true
		}
	}
	return 0, false
}

// Query returns samples for one kept timespan, oldest first. Unknown spans
// have no rows. atMin/atMax are inclusive unix seconds when the matching
// has* flag is set.
func (r *Resources) Query(span int, atMin, atMax int64, exact int64, hasExact, hasMin, hasMax bool) []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	tier := r.tiers[span]
	if tier == nil {
		return nil
	}
	out := make([]Sample, 0, len(tier.points))
	for _, p := range tier.points {
		at := p.At.Unix()
		if hasExact && at != exact {
			continue
		}
		if hasMin && at < atMin {
			continue
		}
		if hasMax && at > atMax {
			continue
		}
		out = append(out, p)
	}
	return out
}

// BucketStart is the inclusive start of the timespan bucket that contains t
// in loc (the server's local zone).
func BucketStart(t time.Time, span int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	t = t.In(loc)
	switch span {
	case SpanSec:
		return t.Truncate(time.Second)
	case SpanHour:
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
	case SpanDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	case SpanWeek:
		wd := t.Weekday()
		shift := int(wd - time.Monday)
		if wd == time.Sunday {
			shift = 6
		}
		day := t.AddDate(0, 0, -shift)
		return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	case SpanMonth:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	case SpanYear:
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, loc)
	default:
		return t.Truncate(time.Second)
	}
}

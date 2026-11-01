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

// Sample is one host/process observation at a bucket start. n is the number
// of observations folded into the averages and is not on the wire.
type Sample struct {
	At         time.Time
	HostCPU    float64
	ProcessCPU float64
	HostMem    float64
	ProcessMem float64
	n          int
}

type cpuSnap struct {
	total, idle, proc uint64
	wall              time.Time
}

type cpuState struct {
	mu   sync.Mutex
	last cpuSnap
	have bool
}

// Resources holds a bounded ring of host samples for this node. Clustered
// nodes each sample themselves; a client reads the node it connected to.
type Resources struct {
	now    func() time.Time
	loc    *time.Location
	sample func() (Sample, bool)
	cpu    cpuState

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
	r := &Resources{now: opts.Now, loc: opts.Loc, tiers: map[int]*resourceTier{}}
	if opts.Sample != nil {
		r.sample = opts.Sample
	} else {
		r.sample = r.hostSample
	}
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

// Add records a sample into every kept timespan using a weighted average.
func (r *Resources) Add(s Sample) {
	if s.At.IsZero() {
		s.At = r.now()
	}
	if s.n <= 0 {
		s.n = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, span := range resourceOrder {
		r.addLocked(span, s)
	}
}

func (r *Resources) addLocked(span int, s Sample) {
	s.At = BucketStart(s.At, span, r.loc)
	tier := r.tiers[span]
	if tier == nil {
		return
	}
	if n := len(tier.points); n > 0 && tier.points[n-1].At.Equal(s.At) {
		tier.points[n-1] = mergeResource(tier.points[n-1], s)
	} else {
		tier.points = append(tier.points, s)
	}
	r.dropOldLocked(span)
}

func (r *Resources) dropOldLocked(span int) {
	tier := r.tiers[span]
	if span == SpanSec {
		cutoff := r.now().Add(-time.Hour)
		i := 0
		for i < len(tier.points) && tier.points[i].At.Before(cutoff) {
			i++
		}
		if i > 0 {
			tier.points = tier.points[i:]
		}
		return
	}
	capN := resourceCaps[span]
	if len(tier.points) > capN {
		tier.points = tier.points[len(tier.points)-capN:]
	}
}

func mergeResource(a, b Sample) Sample {
	na, nb := a.n, b.n
	if na <= 0 {
		na = 1
	}
	if nb <= 0 {
		nb = 1
	}
	n := na + nb
	fn, fa, fb := float64(n), float64(na), float64(nb)
	return Sample{
		At:         a.At,
		HostCPU:    (a.HostCPU*fa + b.HostCPU*fb) / fn,
		ProcessCPU: (a.ProcessCPU*fa + b.ProcessCPU*fb) / fn,
		HostMem:    (a.HostMem*fa + b.HostMem*fb) / fn,
		ProcessMem: (a.ProcessMem*fa + b.ProcessMem*fb) / fn,
		n:          n,
	}
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

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

package livetv

import (
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/faillog"
)

// MaintenanceDiagnostics retains every recovered panic even when repeated
// stack logging is throttled. The owner diagnostics endpoint exposes it.
type MaintenanceDiagnostics struct {
	PanicCount       uint64    `json:"panicCount"`
	LastPanicAt      time.Time `json:"lastPanicAt,omitzero"`
	LastPanicSite    string    `json:"lastPanicSite,omitempty"`
	LastPanicMessage string    `json:"lastPanicMessage,omitempty"`
}
type maintenanceHealth struct {
	sync.Mutex
	state MaintenanceDiagnostics
}

func (s *Service) MaintenanceDiagnostics() MaintenanceDiagnostics {
	s.maintenance.Lock()
	defer s.maintenance.Unlock()
	return s.maintenance.state
}

// Each call site owns its failure tracker; a successful expiry must not reset
// throttling for a repeatedly panicking renderer. Recovery is only a safety
// net: the stack and cumulative diagnostics remain visible to the owner.
func (s *Service) maintenanceSteps() func(string, func()) {
	trackers := map[string]*faillog.Tracker{}
	return func(site string, step func()) {
		tracker := trackers[site]
		if tracker == nil {
			tracker = faillog.New(s.logger(), "livetv maintenance "+site)
			trackers[site] = tracker
		}
		s.maintenanceStep(site, tracker, step)
	}
}
func (s *Service) maintenanceStep(site string, tracker *faillog.Tracker, step func()) {
	defer func() {
		if value := recover(); value != nil {
			message := fmt.Sprint(value)
			if len(message) > 2048 {
				message = message[:2048]
			}
			now := time.Now()
			if s.o.Now != nil {
				now = s.o.Now()
			}
			s.maintenance.Lock()
			s.maintenance.state.PanicCount++
			s.maintenance.state.LastPanicAt = now
			s.maintenance.state.LastPanicSite = site
			s.maintenance.state.LastPanicMessage = message
			s.maintenance.Unlock()
			tracker.Fail(fmt.Errorf("panic: %s", message), "site", site, "stack", string(debug.Stack()))
		}
	}()
	step()
	tracker.OK()
}

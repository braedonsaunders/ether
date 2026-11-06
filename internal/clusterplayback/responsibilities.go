package clusterplayback

import "context"

// ResponsibilityCounts contains no session/operation credentials, private
// addresses or viewer identity. These are outstanding barriers, not a count of
// distinct playbacks. A placement and its encoder may appear independently.
type ResponsibilityCounts struct {
	SourcePreparations, PreparationLifetimes, PreparationReceipts int
	SourceLifetimes, SourceCallbacks                              int
	EncoderStarts, EncoderTokens, Inputs                          int
	RemotePreparing, RemoteLive, RemoteRetiring                   int
	Executions, Reopens, Placements                               int
}

func (c ResponsibilityCounts) Total() int {
	return c.SourcePreparations + c.PreparationLifetimes + c.PreparationReceipts + c.SourceLifetimes + c.SourceCallbacks + c.EncoderStarts + c.EncoderTokens + c.Inputs + c.RemotePreparing + c.RemoteLive + c.RemoteRetiring + c.Executions + c.Reopens + c.Placements
}

// Pending counts unresolved barriers, including expired reservations and joined
// source callback obligations. A read error is never quiescence.
func (r *Runtime) Pending(ctx context.Context) (int, error) {
	counts, err := r.PendingDetails(ctx)
	return counts.Total(), err
}

func (p *RemotePlayback) responsibilities() (preparing, live, retiring int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.entries {
		if entry.retired {
			continue
		}
		switch {
		case entry.preparing:
			preparing++
		case entry.retiring:
			retiring++
		default:
			live++
		}
	}
	return
}

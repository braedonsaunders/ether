package sessions

// Coordination holds the v39 playback owner history and lifecycle gates
// shared by every protocol adapter in one Ether process.
type Coordination struct {
	Owners *PlaybackOwners
	Gates  *Lifecycle
}

func NewCoordination() *Coordination {
	return &Coordination{Owners: NewPlaybackOwners(), Gates: NewLifecycle()}
}

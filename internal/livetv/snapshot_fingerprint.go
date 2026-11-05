package livetv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// snapshotInputFingerprint identifies programming, independently of lineup
// presentation and access metadata. ID is deliberately omitted: ordinary
// programme IDs are derived when reading compact rows for their channel.
func snapshotInputFingerprint(c Channel, revision uint64) string {
	input := struct {
		Revision                  uint64
		Kind                      string
		Epoch                     time.Time
		Seed                      int64
		Shuffle                   bool
		Items                     []Item
		Blocks                    []Block
		Pins                      []PinnedSlot
		Sources                   []ProgramSource
		Collection                core.RatingKey
		Filter                    *core.Filter
		Watched, Mode, Align, Fit string
		RepeatDays                int
		Filler, Fallback          []Item
		OffAir                    bool
		MarathonKey               core.RatingKey
		Timezone, Rating          string
		Clock                     []ClockSlot
		Personal                  *PersonalRecipe
		Look                      *ChannelLook
		Screens                   *ChannelScreens
	}{revision, c.Kind, c.Epoch, c.Seed, c.Shuffle, c.Items, c.Blocks, c.Pins, c.Sources, c.Collection, c.Filter, c.Watched, c.Mode, c.Align, c.Fit, c.RepeatDays, c.Filler, c.Fallback, c.OffAir, c.MarathonKey, c.Timezone, c.Rating, c.Clock, c.Personal, c.Look, c.Screens}
	raw, err := json.Marshal(input)
	if err != nil {
		return "invalid:" + c.ID
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// snapshotFingerprint adds only cached dependencies. No provider, rendering,
// schedule expansion, or catalogue scan is performed while comparing inputs.
func (s *Service) snapshotFingerprint(c Channel, revision uint64, now time.Time) string {
	return s.snapshotFingerprintAt(c, revision, s.snapshotAvailabilityEpoch(now))
}

func (s *Service) snapshotAvailabilityEpoch(now time.Time) uint64 {
	var availability uint64
	if source, ok := s.o.Availability.(interface{ AvailabilityEpoch() uint64 }); ok {
		availability = source.AvailabilityEpoch()
	} else if s.o.Availability != nil {
		// Third-party sources without an invalidation capability retain the former
		// thirty-second availability refresh. The built-in resolver exposes epochs.
		availability = uint64(now.Unix() / 30)
	}
	return availability
}

func (s *Service) snapshotFingerprintAt(c Channel, revision, availability uint64) string {
	type durationInput struct {
		Look, Moment, Name, Rating string
		Duration                   time.Duration
		Available, Metadata        bool
	}
	type reelInput struct {
		ID       string
		Snapshot *ReelSnapshot
	}
	deps := struct {
		Programming  string
		Availability uint64
		DefaultStyle string
		Durations    []durationInput
		Reels        []reelInput
	}{Programming: snapshotInputFingerprint(c, revision), Availability: availability}
	if s.o.DefaultStyle != nil {
		deps.DefaultStyle = s.o.DefaultStyle()
	}
	looks := []*ChannelLook{c.Look}
	for _, b := range c.Blocks {
		looks = append(looks, b.Breaks)
	}
	for _, p := range c.Pins {
		looks = append(looks, p.Breaks)
	}
	seenLook, seenReel, seenSegment := map[string]bool{}, map[string]bool{}, map[string]bool{}
	segment := func(id string) {
		if seenSegment[id] || s.o.Graphics == nil {
			return
		}
		seenSegment[id] = true
		duration, ok := s.o.Graphics.Duration("", id)
		d := durationInput{Moment: id, Duration: duration, Available: ok}
		if metadata, ok := s.o.Graphics.(core.ChannelSegmentMetadata); ok {
			d.Name, d.Rating, d.Metadata = metadata.SegmentInfo(id)
		}
		deps.Durations = append(deps.Durations, d)
	}
	for _, look := range looks {
		if look == nil {
			continue
		}
		lookID := look.LookID
		if lookID == "" {
			lookID = deps.DefaultStyle
		}
		if s.o.Graphics != nil && !seenLook[lookID] {
			seenLook[lookID] = true
			for _, moment := range []string{"ident", "upnext", "tonight", "break", "back", "offair", "welcome"} {
				duration, ok := s.o.Graphics.Duration(lookID, moment)
				deps.Durations = append(deps.Durations, durationInput{Look: lookID, Moment: moment, Duration: duration, Available: ok})
			}
		}
		if look.ReelID != "" && !seenReel[look.ReelID] {
			seenReel[look.ReelID] = true
			s.mu.Lock()
			reel := s.reelSnapshots[look.ReelID]
			s.mu.Unlock()
			deps.Reels = append(deps.Reels, reelInput{ID: look.ReelID, Snapshot: reel})
		}
		for _, id := range look.SegmentIDs {
			segment(id)
		}
	}
	// A configured default style applies even when the saved channel has no Look.
	if c.Look == nil && deps.DefaultStyle != "" && s.o.Graphics != nil && !seenLook[deps.DefaultStyle] {
		for _, moment := range []string{"ident", "upnext", "tonight", "break", "back", "offair", "welcome"} {
			duration, ok := s.o.Graphics.Duration(deps.DefaultStyle, moment)
			deps.Durations = append(deps.Durations, durationInput{Look: deps.DefaultStyle, Moment: moment, Duration: duration, Available: ok})
		}
	}
	for _, slot := range c.Clock {
		segment(slot.SegmentID)
	}
	raw, _ := json.Marshal(deps)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

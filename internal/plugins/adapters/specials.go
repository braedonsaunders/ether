package adapters

import "github.com/braedonsaunders/ether/internal/stremio"

// SetSpecialEpisodes shares the injected IMDb specials mapper with installed
// add-ons. No mapper means specials with IMDb routes are skipped.
func (s *StreamSources) SetSpecialEpisodes(mapper stremio.SpecialEpisodes) {
	s.mu.Lock()
	s.specials = mapper
	s.mu.Unlock()
}

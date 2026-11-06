package plex

import "github.com/braedonsaunders/ether/internal/plex/model"

// Session rows share the decision serializer's string identifiers and sorted
// attributes, including nested media. The catalogue/detail models stay intact.
func sessionItem(md *model.Metadata) {
	decisionItem(md)
	for i := range md.Media {
		m := &md.Media[i]
		m.SetAlphabetical()
		for j := range m.Part {
			p := &m.Part[j]
			p.SetAlphabetical()
			for k := range p.Stream {
				p.Stream[k].SetAlphabetical()
			}
		}
	}
}

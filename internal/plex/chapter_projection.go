package plex

import "github.com/braedonsaunders/ether/internal/plex/model"

func omitChapterRows(md *model.Metadata) {
	md.Chapter = nil
	if md.Related != nil {
		for i := range md.Related.Hub {
			for j := range md.Related.Hub[i].Metadata {
				omitChapterRows(&md.Related.Hub[i].Metadata[j])
			}
		}
	}
	if md.Extras != nil {
		for i := range md.Extras.Metadata {
			omitChapterRows(&md.Extras.Metadata[i])
		}
	}
	if md.Children != nil {
		for i := range md.Children.Metadata {
			omitChapterRows(&md.Children.Metadata[i])
		}
	}
}

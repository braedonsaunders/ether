package resolver

import "github.com/braedonsaunders/ether/internal/core"

func specialShowTitles(req core.SearchRequest) []string {
	return append([]string{req.Title, req.OriginalTitle}, req.AlternativeTitles...)
}

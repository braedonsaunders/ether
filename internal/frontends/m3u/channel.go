package m3u

import (
	"net/http"
	"strings"
)

func validLineupCapability(path string) bool {
	rest, ok := strings.CutPrefix(path, "/sessions/lineup/")
	if !ok {
		return false
	}
	token, ok := strings.CutSuffix(rest, "/")
	if !ok || token == "" || len(token) > 128 {
		return false
	}
	for _, r := range token {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (i *instance) serveLazy(w http.ResponseWriter, r *http.Request) {
	if i.host.LazyStreams == nil {
		http.NotFound(w, r)
		return
	}
	i.host.LazyStreams.ServeHTTP(w, r)
}

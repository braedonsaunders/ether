package plex

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/language"
)

var requestLanguageWeight = regexp.MustCompile(`^(0(?:\.[0-9]{0,3})?|1(?:\.0{0,3})?)$`)
var requestLanguageTag = regexp.MustCompile(`^[A-Za-z]{2,8}(?:-[A-Za-z0-9]{1,8})*$`)

// requestLanguage keeps locale identity for language names while choosing only
// catalogues the server can render. Invalid weighted lists fall back as a unit.
func requestLanguage(r *http.Request) string {
	if raw := plexParam(r, "X-Plex-Language"); raw != "" {
		return chooseRequestLanguage(raw, false)
	}
	if raw := plexParam(r, "X-Plex-Accept-Language"); raw != "" {
		return chooseRequestLanguage(raw, true)
	}
	if raw := r.Header.Get("Accept-Language"); raw != "" {
		return chooseRequestLanguage(raw, true)
	}
	return ""
}

func chooseRequestLanguage(raw string, accepting bool) string {
	best := "en"
	bestRank := -1
	bestWeight := -1.0
	for _, member := range strings.Split(raw, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		fields := strings.Split(member, ";")
		tag := strings.TrimSpace(fields[0])
		if len(fields) > 2 || tag == "" || (tag != "*" && !requestLanguageTag.MatchString(tag)) {
			return "en"
		}
		weight := 1.0
		if len(fields) == 2 {
			parameter := strings.TrimSpace(fields[1])
			value, ok := strings.CutPrefix(parameter, "q=")
			if !ok || !requestLanguageWeight.MatchString(value) {
				return "en"
			}
			weight, _ = strconv.ParseFloat(value, 64)
		}
		if weight == 0 || tag == "*" {
			continue
		}
		parsed, err := language.Parse(tag)
		if err != nil {
			continue
		}
		base, _ := parsed.Base()
		code := base.String()
		if accepting && traditionalChinese(tag) {
			continue
		}
		rank := 2
		if code == "ca" || code == "nn" {
			rank = 1
		}
		if !strings.Contains("|af|ar|bg|cs|da|de|el|en|es|et|fa|fi|fr|he|hr|hu|id|is|it|ja|ko|lt|lv|ms|my|nb|nl|nn|no|pl|pt|ro|ru|sk|sr|sv|th|tr|uk|vi|zh|ca|", "|"+code+"|") {
			continue
		}
		if rank > bestRank || rank == bestRank && weight > bestWeight {
			best, bestRank, bestWeight = tag, rank, weight
		}
	}
	return best
}

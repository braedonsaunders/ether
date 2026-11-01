package silo

import (
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func peopleProblem(w http.ResponseWriter, status int) {
	problemReply(w, status, map[string]any{"type": "urn:ether:problem:people-unavailable", "title": http.StatusText(status), "status": status, "detail": "The requested people operation is unavailable.", "instance": "/api/v2/catalog/people"})
}
func (i *instance) personDTO(r *http.Request, credential string, person frontend.PersonDetail) map[string]any {
	out := map[string]any{"id": person.ID, "name": person.Name}
	if person.TMDB > 0 {
		out["tmdb_id"] = strconv.FormatInt(person.TMDB, 10)
	}
	if person.IMDB != "" {
		out["imdb_id"] = person.IMDB
	}
	if person.ArtworkItem != 0 && i.host.ImageGrants != nil {
		for _, image := range person.Images {
			if image.Kind != frontend.ImagePerson || image.Subject != "tmdb:"+strconv.FormatInt(person.TMDB, 10) || image.Path == "" {
				continue
			}
			token, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: credential, Device: device(r), Item: person.ArtworkItem, Image: image})
			if err != nil {
				continue
			}
			prefix := ""
			if r.Context().Value(sharedPrefix{}) != nil {
				prefix = "/silo"
			}
			out["photo_url"] = prefix + "/api/v2/images/" + token
			if image.ThumbHash != "" {
				out["photo_thumbhash"] = image.ThumbHash
			}
			break
		}
	}
	return out
}
func peopleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, frontend.ErrPeopleQuery):
		peopleProblem(w, 400)
	case errors.Is(err, frontend.ErrNotFound):
		peopleProblem(w, 404)
	default:
		peopleProblem(w, 503)
	}
}
func (i *instance) people(w http.ResponseWriter, r *http.Request) {
	viewer, credential, err := i.viewer(r)
	if err != nil || viewer == nil {
		peopleProblem(w, 401)
		return
	}
	if r.Header.Get("X-Profile-Id") == "" {
		peopleProblem(w, 422)
		return
	}
	reader, ok := i.host.Experience.(frontend.PeopleReader)
	if !ok {
		peopleProblem(w, 503)
		return
	}
	q := r.URL.Query()
	limit := 20
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			peopleProblem(w, 400)
			return
		}
	}
	if utf8.RuneCountInString(q.Get("q")) > 200 {
		peopleProblem(w, 400)
		return
	}
	scope := frontend.PeopleScope(q.Get("media_scope"))
	switch scope {
	case frontend.PeopleAll:
		if q.Has("media_scope") {
			peopleProblem(w, 400)
			return
		}
	case frontend.PeopleVideo, frontend.PeopleVideoWithEpisodes, frontend.PeopleMovies, frontend.PeopleSeries, frontend.PeopleEpisodes:
	default:
		peopleProblem(w, 400)
		return
	}
	people, err := reader.People(r.Context(), viewer, device(r), frontend.PeopleQuery{Query: q.Get("q"), Limit: limit, Scope: scope})
	if err != nil {
		peopleError(w, err)
		return
	}
	items := make([]any, 0, len(people))
	for _, person := range people {
		items = append(items, i.personDTO(r, credential, person))
	}
	reply(w, 200, map[string]any{"items": items})
}
func (i *instance) person(w http.ResponseWriter, r *http.Request) {
	viewer, credential, err := i.viewer(r)
	if err != nil || viewer == nil {
		peopleProblem(w, 401)
		return
	}
	if r.Header.Get("X-Profile-Id") == "" {
		peopleProblem(w, 422)
		return
	}
	reader, ok := i.host.Experience.(frontend.PeopleReader)
	if !ok {
		peopleProblem(w, 503)
		return
	}
	prefetch := false
	if r.URL.Query().Has("prefetch") {
		switch r.URL.Query().Get("prefetch") {
		case "true":
			prefetch = true
		case "false":
		default:
			peopleProblem(w, 400)
			return
		}
	}
	person, err := reader.Person(r.Context(), viewer, device(r), r.PathValue("person"), frontend.PersonReadOptions{Prefetch: prefetch})
	if err == nil && (person.ID != r.PathValue("person") || person.Name == "") {
		peopleProblem(w, 503)
		return
	}
	if err != nil {
		peopleError(w, err)
		return
	}
	reply(w, 200, i.personDTO(r, credential, person))
}
func (i *instance) searchCapabilities(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		peopleProblem(w, 401)
		return
	}
	if r.Header.Get("X-Profile-Id") == "" {
		peopleProblem(w, 422)
		return
	}
	_, supported := i.host.Experience.(frontend.PeopleReader)
	state := "unsupported"
	if supported {
		state = "available"
	}
	// The episode flag also promises query-source catalogue browse, which this
	// adapter does not yet serve. Person membership alone cannot assert it.
	reply(w, 200, map[string]any{"revision": "ether-people-v1-" + strconv.FormatBool(supported), "state": state, "allowed": supported, "people_media_scope": supported, "person_prefetch": supported, "video_with_episodes_scope": false})
}
func (i *instance) peopleRoutes() error {
	for _, route := range []struct {
		path    string
		handler http.HandlerFunc
	}{{"/api/v2/catalog/people", i.people}, {"/api/v2/catalog/people/{person}", i.person}, {"/api/v2/catalog/search/capabilities", i.searchCapabilities}} {
		if err := i.router.Handle("GET", route.path, route.handler); err != nil {
			return err
		}
	}
	return nil
}

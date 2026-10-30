package experience

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const peopleReadBudget = 4 * time.Second

type visiblePerson struct {
	detail      frontend.PersonDetail
	hasPortrait bool
}

func peopleTypes(scope frontend.PeopleScope) ([]core.MediaType, error) {
	switch scope {
	case frontend.PeopleAll, frontend.PeopleVideoWithEpisodes:
		return []core.MediaType{core.TypeMovie, core.TypeShow, core.TypeEpisode}, nil
	case frontend.PeopleVideo:
		return []core.MediaType{core.TypeMovie, core.TypeShow}, nil
	case frontend.PeopleMovies:
		return []core.MediaType{core.TypeMovie}, nil
	case frontend.PeopleSeries:
		return []core.MediaType{core.TypeShow}, nil
	case frontend.PeopleEpisodes:
		return []core.MediaType{core.TypeEpisode}, nil
	default:
		return nil, frontend.ErrPeopleQuery
	}
}

// Membership comes only from cached credit rows. Current cached title and
// inherited-show policy are checked before any credit is matched or ranked.
func (s *Service) peopleVisibility(ctx context.Context, titles map[core.RatingKey]core.Title) (Visibility, error) {
	parents := []core.RatingKey{}
	seen := map[core.RatingKey]bool{}
	for _, title := range titles {
		if title.ShowKey != 0 && !seen[title.ShowKey] {
			seen[title.ShowKey] = true
			parents = append(parents, title.ShowKey)
		}
	}
	all := make(map[core.RatingKey]core.Title, len(titles)+len(parents))
	for key, title := range titles {
		all[key] = title
	}
	if len(parents) > 0 {
		loaded, err := s.d.CachedGetMany(ctx, parents)
		if err != nil {
			return Visibility{}, err
		}
		for key, title := range loaded {
			all[key] = title
		}
	}
	visibility := s.d.Visibility
	visibility.GetTitle = func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		title, ok := all[key]
		if !ok {
			return nil, core.ErrNotFound
		}
		return &title, nil
	}
	return visibility, nil
}
func peopleTitleVisible(ctx context.Context, v *core.Viewer, title *core.Title, visibility Visibility) bool {
	if title == nil || visibility.SectionFor == nil {
		return false
	}
	section, ok := visibility.SectionFor(title.Type)
	return ok && SectionAllowed(v, section) && visibility.Visible(ctx, v, title)
}
func (s *Service) peopleTitle(ctx context.Context, viewer *core.Viewer, key core.RatingKey) (*core.Title, error) {
	if s.d.CachedGetMany == nil {
		return nil, core.ErrNotAvailable
	}
	titles, err := s.d.CachedGetMany(ctx, []core.RatingKey{key})
	if err != nil {
		return nil, err
	}
	title, ok := titles[key]
	if !ok || title.Key != key {
		return nil, core.ErrNotFound
	}
	visibility, err := s.peopleVisibility(ctx, titles)
	if err != nil {
		return nil, err
	}
	if !peopleTitleVisible(ctx, viewer, &title, visibility) {
		return nil, core.ErrNotFound
	}
	return &title, nil
}

func (s *Service) visitPeople(ctx context.Context, viewer *core.Viewer, device core.Device, scope frontend.PeopleScope, includeSeasons bool, visit func(core.Person, core.RatingKey) error) error {
	types, err := peopleTypes(scope)
	if err != nil {
		return err
	}
	if includeSeasons {
		types = append(types, core.TypeSeason)
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return err
	}
	if s.d.CreditItems == nil || s.d.CachedGetMany == nil {
		return core.ErrNotAvailable
	}
	for after := core.RatingKey(0); ; {
		keys, err := s.d.CreditItems(ctx, after, 128)
		if err != nil {
			return err
		}
		if len(keys) > 128 {
			return core.ErrNotAvailable
		}
		previous := after
		selected := make([]core.RatingKey, 0, len(keys))
		for _, key := range keys {
			if key <= previous {
				return core.ErrNotAvailable
			}
			previous = key
			wanted := false
			for _, kind := range types {
				if (kind == core.TypeMovie && key.Kind() == core.KindMovie) || (kind == core.TypeShow && key.Kind() == core.KindShow) || (kind == core.TypeEpisode && key.Kind() == core.KindEpisode) || (kind == core.TypeSeason && key.Kind() == core.KindSeason) {
					wanted = true
					break
				}
			}
			if wanted {
				selected = append(selected, key)
			}
		}
		titles, err := s.d.CachedGetMany(ctx, selected)
		if err != nil {
			return err
		}
		visibility, err := s.peopleVisibility(ctx, titles)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err = ctx.Err(); err != nil {
				return err
			}
			title, ok := titles[key]
			if !ok || title.Key != key {
				continue
			}
			include := false
			for _, kind := range types {
				if title.Type == kind {
					include = true
					break
				}
			}
			if !include || !peopleTitleVisible(ctx, v, &title, visibility) {
				continue
			}
			for _, group := range [][]core.Person{title.Cast, title.Directors, title.Writers, title.Crew} {
				for _, person := range group {
					if person.TMDB <= 0 || strings.TrimSpace(person.Name) == "" {
						continue
					}
					if err = visit(person, title.Key); err != nil {
						return err
					}
				}
			}
		}
		if len(keys) < 128 {
			return nil
		}
		after = keys[len(keys)-1]
	}
}

func mergeVisiblePerson(old visiblePerson, person core.Person, item core.RatingKey) (visiblePerson, error) {
	id := facetID("person", strconv.FormatInt(person.TMDB, 10))
	if old.detail.ID != "" && old.detail.TMDB != person.TMDB {
		return old, core.ErrNotAvailable
	}
	candidate := visiblePerson{detail: frontend.PersonDetail{ID: id, Name: person.Name, TMDB: person.TMDB, IMDB: person.IMDB, ArtworkItem: item}, hasPortrait: person.ThumbPath != ""}
	if old.detail.ID == "" {
		return candidate, nil
	}
	// Keep displayed facts and the artwork owner on the same visible credit.
	left, right := strings.Join(peopleWords(person.Name), " "), strings.Join(peopleWords(old.detail.Name), " ")
	better := left < right || (left == right && ((candidate.hasPortrait && !old.hasPortrait) || (candidate.hasPortrait == old.hasPortrait && item < old.detail.ArtworkItem)))
	if better {
		return candidate, nil
	}
	return old, nil
}

func peopleWords(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}
func peopleTerms(value string) []string {
	seen := map[string]bool{}
	terms := []string{}
	for _, word := range peopleWords(value) {
		if !seen[word] {
			seen[word] = true
			terms = append(terms, word)
			if len(terms) == 8 {
				break
			}
		}
	}
	return terms
}
func personMatches(name string, terms []string) bool {
	words := peopleWords(name)
	for _, term := range terms {
		found := false
		for _, word := range words {
			if strings.HasPrefix(word, term) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func personBefore(a, b frontend.PersonDetail, exact string) bool {
	left, right := strings.Join(peopleWords(a.Name), " "), strings.Join(peopleWords(b.Name), " ")
	if (left == exact) != (right == exact) {
		return left == exact
	}
	if left != right {
		return left < right
	}
	return a.ID < b.ID
}

// Recheck the chosen visible credit before returning metadata; optional facts
// are read from that same current credit rather than a stale aggregate.
func (s *Service) currentPerson(ctx context.Context, viewer *core.Viewer, person frontend.PersonDetail) (frontend.PersonDetail, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.PersonDetail{}, err
	}
	title, err := s.peopleTitle(ctx, v, person.ArtworkItem)
	if err != nil {
		return frontend.PersonDetail{}, err
	}
	var current visiblePerson
	for _, group := range [][]core.Person{title.Cast, title.Directors, title.Writers, title.Crew} {
		for _, credit := range group {
			if credit.TMDB == person.TMDB && credit.Name != "" {
				current, err = mergeVisiblePerson(current, credit, title.Key)
				if err != nil {
					return frontend.PersonDetail{}, err
				}
			}
		}
	}
	if current.detail.ID != person.ID {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	return current.detail, nil
}

func (s *Service) People(ctx context.Context, viewer *core.Viewer, device core.Device, q frontend.PeopleQuery) ([]frontend.PersonDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, peopleReadBudget)
	defer cancel()
	if utf8.RuneCountInString(q.Query) > 200 || q.Limit < 0 || q.Limit > 100 {
		return nil, frontend.ErrPeopleQuery
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	terms := peopleTerms(q.Query)
	exact := strings.Join(peopleWords(q.Query), " ")
	// Keep only the requested best people while scanning authorized credits.
	// No persistent/global mapping or unbounded result set is accumulated here.
	best := make(map[string]visiblePerson, q.Limit)
	err := s.visitPeople(ctx, viewer, device, q.Scope, false, func(person core.Person, item core.RatingKey) error {
		if !personMatches(person.Name, terms) {
			return nil
		}
		id := facetID("person", strconv.FormatInt(person.TMDB, 10))
		candidate, err := mergeVisiblePerson(best[id], person, item)
		if err != nil {
			return err
		}
		if _, exists := best[id]; exists || len(best) < q.Limit {
			best[id] = candidate
			return nil
		}
		worst := ""
		for key, value := range best {
			if worst == "" || personBefore(best[worst].detail, value.detail, exact) {
				worst = key
			}
		}
		if personBefore(candidate.detail, best[worst].detail, exact) {
			delete(best, worst)
			best[id] = candidate
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]frontend.PersonDetail, 0, len(best))
	for _, person := range best {
		current, err := s.currentPerson(ctx, viewer, person.detail)
		if err != nil {
			return nil, err
		}
		if personMatches(current.Name, terms) {
			result = append(result, current)
		}
	}
	sort.Slice(result, func(a, b int) bool { return personBefore(result[a], result[b], exact) })
	return result, nil
}

func (s *Service) Person(ctx context.Context, viewer *core.Viewer, device core.Device, id string, _ frontend.PersonReadOptions) (frontend.PersonDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, peopleReadBudget)
	defer cancel()
	if id == "" || len(id) > 128 {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	var found visiblePerson
	err := s.visitPeople(ctx, viewer, device, frontend.PeopleAll, true, func(person core.Person, item core.RatingKey) error {
		if facetID("person", strconv.FormatInt(person.TMDB, 10)) != id {
			return nil
		}
		var err error
		found, err = mergeVisiblePerson(found, person, item)
		return err
	})
	if err != nil {
		return frontend.PersonDetail{}, err
	}
	if found.detail.ID == "" {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	return s.currentPerson(ctx, viewer, found.detail)
}

// PersonCredit revalidates one cached, title-bound person without playback,
// provider refresh, or an unfiltered catalogue fallback.
func (s *Service) PersonCredit(ctx context.Context, viewer *core.Viewer, _ core.Device, item core.RatingKey, tmdb int64) (frontend.PersonDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, peopleReadBudget)
	defer cancel()
	if tmdb <= 0 {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	return s.currentPerson(ctx, viewer, frontend.PersonDetail{ID: facetID("person", strconv.FormatInt(tmdb, 10)), TMDB: tmdb, ArtworkItem: item})
}

var _ frontend.PeopleReader = (*Service)(nil)

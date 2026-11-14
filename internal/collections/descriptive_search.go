package collections

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

const describeCacheMax = 256

type describeDraft struct {
	done chan struct{}
	spec *core.CollectionSpec
	err  error
	at   time.Time
}
type describeJob struct {
	done   chan struct{}
	result core.DescriptiveResult
	err    error
	at     time.Time
}

func (s *Service) DescriptiveSearchAvailable() bool {
	chat := s.chatDep()
	_, ok := s.cat.(core.DescriptiveCatalog)
	return ok && chat != nil && chat.Enabled()
}

// DescribeSearch schedules at most two jobs. Cache hits never call the model.
// Drafts are shared by normalized query; results and taste are viewer scoped.
// Errors back off for a minute. Maps are lazy and bounded, with no sweeper.
func (s *Service) DescribeSearch(ctx context.Context, user *core.User, query string, wait bool) (core.DescriptiveResult, error) {
	if !s.DescriptiveSearchAvailable() {
		return core.DescriptiveResult{State: "unavailable"}, nil
	}
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 1000 {
		return core.DescriptiveResult{}, fmt.Errorf("invalid descriptive search")
	}
	if err := ctx.Err(); err != nil {
		return core.DescriptiveResult{}, err
	}
	normalized := strings.Join(strings.Fields(strings.ToLower(query)), " ")
	key := fmt.Sprintf("%d|%s", vibeOwner(user), normalized)
	s.describeMu.Lock()
	job := s.describeJobs[key]
	if job != nil {
		select {
		case <-job.done:
			ttl := 10 * time.Minute
			if job.err != nil {
				ttl = time.Minute
			}
			if s.now().Sub(job.at) >= ttl {
				delete(s.describeJobs, key)
				job = nil
			}
		default:
		}
	}
	if job == nil {
		if s.describeActive >= 2 {
			s.describeMu.Unlock()
			return core.DescriptiveResult{State: "busy"}, nil
		}
		if s.describeJobs == nil {
			s.describeJobs = map[string]*describeJob{}
		}
		if len(s.describeJobs) >= describeCacheMax {
			for k, j := range s.describeJobs {
				select {
				case <-j.done:
					delete(s.describeJobs, k)
				default:
				}
				if len(s.describeJobs) < describeCacheMax {
					break
				}
			}
		}
		job = &describeJob{done: make(chan struct{}), at: s.now()}
		s.describeJobs[key] = job
		s.describeActive++
		var viewer *core.User
		if user != nil {
			copy := *user
			viewer = &copy
		}
		go s.runDescribe(context.WithoutCancel(ctx), viewer, normalized, query, job)
	}
	s.describeMu.Unlock()
	if !wait {
		select {
		case <-job.done:
		default:
			return core.DescriptiveResult{State: "pending"}, nil
		}
	}
	select {
	case <-ctx.Done():
		return core.DescriptiveResult{}, ctx.Err()
	case <-job.done:
		result := job.result
		result.Items = slices.Clone(result.Items)
		result.Items = slices.DeleteFunc(result.Items, func(t core.Title) bool {
			return t.Adult || (user != nil && contentrating.Exceeds(t.ContentRating, user.MaxContentRating))
		})
		if result.Spec != nil {
			sp := cloneSpec(*result.Spec)
			result.Spec = &sp
		}
		return result, job.err
	}
}

func (s *Service) describeSpec(ctx context.Context, key, query string) (*core.CollectionSpec, error) {
	s.describeMu.Lock()
	if s.describeDrafts == nil {
		s.describeDrafts = map[string]*describeDraft{}
	}
	draft := s.describeDrafts[key]
	if draft != nil {
		select {
		case <-draft.done:
			ttl := 6 * time.Hour
			if draft.err != nil {
				ttl = time.Minute
			}
			if s.now().Sub(draft.at) >= ttl {
				delete(s.describeDrafts, key)
				draft = nil
			}
		default:
		}
	}
	if draft != nil {
		s.describeMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-draft.done:
			return draft.spec, draft.err
		}
	}
	if len(s.describeDrafts) >= describeCacheMax {
		for k, d := range s.describeDrafts {
			select {
			case <-d.done:
				delete(s.describeDrafts, k)
			default:
			}
			if len(s.describeDrafts) < describeCacheMax {
				break
			}
		}
	}
	draft = &describeDraft{done: make(chan struct{}), at: s.now()}
	s.describeDrafts[key] = draft
	s.describeMu.Unlock()
	// Same generator, SpecGuide and parseDraft used by Describe a collection.
	// Only local sources are useful in a search; no external discovery work.
	spec, _, err := s.Generate(ctx, query+"\nFor this temporary search use the catalog source (or local TMDB person, keyword, studio or franchise sources when needed) and local genre, keyword, year, rating, votes, popularity, runtime, language, country or studio rules. Do not use external lists or per-viewer watch-state rules. Limit to 200 titles.", MediaMixed)
	s.describeMu.Lock()
	draft.spec, draft.err, draft.at = spec, err, s.now()
	close(draft.done)
	s.describeMu.Unlock()
	return spec, err
}

func (s *Service) runDescribe(parent context.Context, user *core.User, key, query string, job *describeJob) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	spec, err := s.describeSpec(ctx, key, query)
	result := core.DescriptiveResult{State: "failed"}
	if err == nil {
		result.Items, err = s.cat.(core.DescriptiveCatalog).DescribeCandidates(core.WithCatalogViewer(core.WithExplicitSearch(ctx), user), *spec, 200)
		if err == nil {
			result.State, result.Title = "ready", spec.Name
			result.Spec = spec
			// Reuse the viewer's existing taste model on the bounded candidate set.
			if sc := s.personalScorer(); sc != nil && user != nil && len(result.Items) > 1 {
				if scores, e := sc.ScoreTitles(ctx, user, result.Items); e == nil {
					slices.SortStableFunc(result.Items, func(a, b core.Title) int {
						x, y := scores[a.Key], scores[b.Key]
						if math.IsNaN(x) || math.IsInf(x, 0) {
							x = 0
						}
						if math.IsNaN(y) || math.IsInf(y, 0) {
							y = 0
						}
						if x > y {
							return -1
						}
						if x < y {
							return 1
						}
						return 0
					})
				}
			}
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result.State = "failed"
	}
	s.describeMu.Lock()
	job.result, job.err, job.at = result, err, s.now()
	s.describeActive--
	close(job.done)
	s.describeMu.Unlock()
}

var _ core.DescriptiveSearcher = (*Service)(nil)

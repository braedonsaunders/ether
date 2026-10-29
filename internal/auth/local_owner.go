package auth

import "context"

// RefreshViewers reloads viewer state after an identity service has made an
// atomic membership change. Cluster peers receive the same invalidation.
func (s *Service) RefreshViewers(ctx context.Context) error {
	if err := s.reload(ctx); err != nil {
		return err
	}
	s.notify("caches")
	return nil
}

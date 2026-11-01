package safety

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// FreshPolicy reads the durable policy without consulting the process cache.
// The caller must fence the corresponding durable viewer-policy epoch across
// authorization; a standalone result is not itself an authorization.
// A missing row has the same unrestricted policy value as Service.Get.
func (s *Service) FreshPolicy(ctx context.Context, viewerID int64) (Policy, error) {
	if s == nil || s.db == nil || viewerID <= 0 {
		return Policy{}, errors.New("safety: invalid viewer")
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT policy FROM safety_policies WHERE user_id=?`, viewerID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, nil
	}
	if err != nil {
		return Policy{}, err
	}
	var policy Policy
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return Policy{}, err
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

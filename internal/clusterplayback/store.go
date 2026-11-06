// Package clusterplayback composes exact source authority and fenced HLS execution.
package clusterplayback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/store"
)

var ErrDenied = errors.New("clusterplayback: denied")
var ErrPending = errors.New("clusterplayback: responsibility pending")

func init() {
	store.Register("clusterplayback", 1, `
CREATE TABLE clusterplayback_routes (session TEXT PRIMARY KEY, generation BIGINT NOT NULL, route TEXT NOT NULL);
`)
	store.Register("clusterplayback", 2, `
CREATE TABLE clusterplayback_preparation_receipts (
 cluster TEXT NOT NULL, source TEXT NOT NULL, source_boot TEXT NOT NULL,
 controller TEXT NOT NULL, controller_boot TEXT NOT NULL, operation_hash TEXT NOT NULL,
 receipt TEXT NOT NULL,
 PRIMARY KEY(cluster,source,source_boot,controller,controller_boot,operation_hash));
`)
}

type Route struct {
	Fence            placement.Fence
	Source           nodewire.Identity
	Viewer           int64
	PlayID, Revision string
}

func (r *Runtime) route(ctx context.Context, id string) (Route, error) {
	var raw string
	var out Route
	err := r.o.DB.QueryRowContext(ctx, `SELECT route FROM clusterplayback_routes WHERE session=?`, id).Scan(&raw)
	if err != nil {
		return out, err
	}
	if json.Unmarshal([]byte(raw), &out) != nil || out.Fence.Session != id {
		return Route{}, ErrDenied
	}
	return out, nil
}
func (r *Runtime) putRoute(ctx context.Context, route Route) error {
	b, e := json.Marshal(route)
	if e != nil {
		return e
	}
	_, e = r.o.DB.ExecContext(ctx, `INSERT INTO clusterplayback_routes(session,generation,route) VALUES(?,?,?) ON CONFLICT(session) DO UPDATE SET generation=excluded.generation,route=excluded.route WHERE clusterplayback_routes.generation<excluded.generation`, route.Fence.Session, route.Fence.Generation, string(b))
	if e != nil {
		return e
	}
	got, e := r.route(ctx, route.Fence.Session)
	if e != nil || got != route {
		return ErrDenied
	}
	return nil
}

// Verifier is constructed before placement.New and bound once after NewRuntime.
// An unbound verifier denies retirement. There is no timeout-based release.
type Verifier struct{ runtime *Runtime }

func NewVerifier() *Verifier { return &Verifier{} }
func (v *Verifier) Bind(r *Runtime) error {
	if v.runtime != nil || r == nil {
		return ErrDenied
	}
	v.runtime = r
	return nil
}
func (v *Verifier) VerifyRetired(ctx context.Context, f placement.Fence, _ string) error {
	if v.runtime == nil {
		return ErrPending
	}
	return v.runtime.verifyRetired(ctx, f)
}

var _ placement.RetirementVerifier = (*Verifier)(nil)
var _ = sql.ErrNoRows

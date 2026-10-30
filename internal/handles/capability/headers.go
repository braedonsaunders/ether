package capability

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// HeaderScope binds a header to its owning domain and authenticated subject.
// The calling adapter must authorize this scope against its domain authority.
type HeaderScope struct {
	Namespace string
	ViewerID  int64
	Surface   string
	DeviceID  string
}

// Header is non-secret bookkeeping; Digest is the domain's existing token hash.
// ExpiresMillis is an absolute Unix timestamp in milliseconds. Domain payloads
// and authorization remain in the domain's own table.
type Header struct {
	HeaderScope
	Digest        string
	ExpiresMillis int64
}

// HeaderBatchMax bounds work per call; it is not a storage quota or a fence
// against old writers. Adapters must enforce their own durable admission limits.
const HeaderBatchMax = 40

var ErrInvalidHeader = errors.New("capability: invalid header or scope")

func validHeaderScope(s HeaderScope) bool {
	return s.Namespace != "" && s.Namespace != ticketNamespace && len(s.Namespace) <= 128 &&
		strings.TrimSpace(s.Namespace) == s.Namespace && s.ViewerID > 0 && s.Surface != "" && len(s.Surface) <= 128 && s.DeviceID != "" && len(s.DeviceID) <= 512
}

// BindHeadersInTx inserts headers on the caller's transaction. The caller owns
// scope authorization, per-domain quota, timestamp selection and atomic domain
// insertion; tx must come from the same database as the domain operation.
// Duplicate namespace/digest fails and must cause the caller to roll back.
func BindHeadersInTx(ctx context.Context, tx *sql.Tx, headers []Header) error {
	if tx == nil || len(headers) == 0 || len(headers) > HeaderBatchMax {
		return ErrInvalidHeader
	}
	for _, h := range headers {
		if !validHeaderScope(h.HeaderScope) || !digestValid(h.Digest) || h.ExpiresMillis <= 0 {
			return ErrInvalidHeader
		}
	}
	for _, h := range headers {
		_, err := tx.ExecContext(ctx, `INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES(?,?,?,?,?,?,?)`, h.Namespace, h.Digest, h.ViewerID%ticketShards, h.ViewerID, h.Surface, h.DeviceID, h.ExpiresMillis)
		if err != nil {
			return err
		}
	}
	return nil
}

// DropHeadersInTx removes only exact scope matches. Unknown or foreign headers
// are left untouched. Callers must authorize their corresponding domain deletion
// in the same transaction and not infer authorization from this operation.
func DropHeadersInTx(ctx context.Context, tx *sql.Tx, scope HeaderScope, digests []string) error {
	if tx == nil || !validHeaderScope(scope) || len(digests) == 0 || len(digests) > HeaderBatchMax {
		return ErrInvalidHeader
	}
	for _, d := range digests {
		if !digestValid(d) {
			return ErrInvalidHeader
		}
	}
	for _, d := range digests {
		_, err := tx.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND digest=? AND viewer_id=? AND surface=? AND device_id=?`, scope.Namespace, d, scope.ViewerID, scope.Surface, scope.DeviceID)
		if err != nil {
			return err
		}
	}
	return nil
}

// PruneHeadersInTx deletes at most limit expired headers in one namespace.
// cutoffMillis must come from the domain's trusted clock and must not precede
// its actual authority lifetime. This only removes mirror bookkeeping; domain
// retention and active-session retirement remain the caller's responsibility.
// No capability, including one considered live by its domain, may rely on this
// header surviving beyond the supplied cutoff. The caller commits or rolls back.
func PruneHeadersInTx(ctx context.Context, tx *sql.Tx, namespace string, cutoffMillis int64, limit int) (int64, error) {
	if tx == nil || namespace == "" || namespace == ticketNamespace || len(namespace) > 128 || strings.TrimSpace(namespace) != namespace || cutoffMillis <= 0 || limit <= 0 || limit > HeaderBatchMax {
		return 0, ErrInvalidHeader
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace=? AND expires<=? AND digest IN (SELECT digest FROM capability_headers WHERE namespace=? AND expires<=? ORDER BY expires,digest LIMIT ?)`, namespace, cutoffMillis, namespace, cutoffMillis, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

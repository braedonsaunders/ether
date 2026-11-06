package clusterplayback

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
)

// initialAdmission is private historical admission state, not an
// output observation or execution permission. Route binds the actual reserved
// full fence and authenticated source owner/PlayID/revision. It carries the
// owning controller identity; there is no separate delivery identity in Route.
// The initial request remains unchanged by backend/live observations. No raw
// input URL, headers, source grant or input-lifetime actor is retained here.
type initialAdmission struct {
	route   Route
	request core.HLSRequest

	// These fields bind the actual installed Start at the durable pre-send seam.
	// The retained owned token independently confirms them after RPC return.
	// They never assert acceptance.
	commitmentIdentity remotetranscode.Identity
	commitment         [32]byte
	hasCommitment      bool
}

// The caller owns q during this copy. A detached request is not synchronization
// for a caller concurrently modifying its input while the copy itself runs.
func cloneAdmissionRequest(q core.HLSRequest) core.HLSRequest {
	q.Info = core.CloneMediaInfo(q.Info)
	q.Decision.AudioFilters = cloneAdmissionStrings(q.Decision.AudioFilters)
	q.SlateText = cloneAdmissionStrings(q.SlateText)
	q.RecoveryStart = core.CloneRecoveryStart(q.RecoveryStart)
	if q.SoftSubtitleStream != nil {
		stream := *q.SoftSubtitleStream
		q.SoftSubtitleStream = &stream
	}
	if q.InputHeaders != nil {
		headers := make(map[string]string, len(q.InputHeaders))
		for key, value := range q.InputHeaders {
			headers[key] = value
		}
		q.InputHeaders = headers
	}
	return q
}

func cloneAdmissionStrings(values []string) []string {
	if values == nil {
		return nil
	}
	clone := make([]string, len(values))
	copy(clone, values)
	return clone
}

func newInitialAdmission(route Route, q core.HLSRequest) *initialAdmission {
	q = cloneAdmissionRequest(q)
	q.InputURL, q.InputHeaders, q.InputLease = "", nil, nil
	return &initialAdmission{route: route, request: q}
}

// retainStartCommitment always retains the exact owned outcome, even if its
// commitment cannot be validated. Getters run outside Runtime.mu; mismatch
// leaves responsibility intact and prevents this start being treated as live.
func (r *Runtime) retainStartCommitment(x *execution, token core.EncoderRetirement) bool {
	session, ok := token.(*remotetranscode.Session)
	var identity remotetranscode.Identity
	var hash [32]byte
	var err error
	if ok {
		identity, hash, err = session.StartCommitment()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	x.retirement = token
	if ok {
		x.session = session
	}
	admission := x.initial
	if !ok || err != nil || admission == nil || x.route != admission.route || identity != remoteIdentity(admission.route) {
		return false
	}
	if admission.hasCommitment && (admission.commitmentIdentity != identity || admission.commitment != hash) {
		return false
	}
	admission.commitmentIdentity, admission.commitment, admission.hasCommitment = identity, hash, true
	return true
}

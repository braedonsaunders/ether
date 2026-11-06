package remotetranscode

import "crypto/sha256"

// StartCommitment returns the identity and hash of the exact immutable marshaled
// Start retained before attempting the start RPC. It exposes no raw request,
// grant or URL. An uncertain, stopped or retired token retains this commitment.
// Success says nothing about sending, worker acceptance, availability, source
// verification or retirement. No network work or state transition occurs here.
func (s *Session) StartCommitment() (Identity, [32]byte, error) {
	if s == nil {
		return Identity{}, [32]byte{}, ErrUnsupported
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validIdentity(s.id) || len(s.start) == 0 || len(s.start) > MaxMessage {
		return Identity{}, [32]byte{}, ErrUnsupported
	}
	return s.id, sha256.Sum256(s.start), nil
}

package derpquic

import "time"

// AuthoritySubject is one endpoint this relay holds authority for: membership only. Grant
// generations change on every peer-network refresh; the control plane fences by min_generation.
type AuthoritySubject struct {
	AccountID  string `json:"account_id"`
	EndpointID string `json:"endpoint_id"`
}

func (s *Server) AuthoritySubjects() []AuthoritySubject {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuthoritySubject, 0, len(s.fences))
	live := s.liveGrantsLocked()
	now := time.Now().Unix()
	for _, f := range s.fences {
		g := f.Grant
		if g.AccountID != "" && (g.ExpiresAt > now || liveIn(live, g)) {
			out = append(out, AuthoritySubject{g.AccountID, g.EndpointID})
		}
	}
	return out
}
func (s *Server) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listener != nil && !s.draining
}

// MatchesControlService gates readiness on the registry's exact public service
// identity; a DERP-only process cannot advertise a missing peer-relay service.
func (s *Server) MatchesControlService(identity *ServiceDescriptor) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if identity == nil {
		return s.service == nil
	}
	return s.service != nil && s.service.identity == *identity
}

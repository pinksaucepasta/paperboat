package derpquic

import "time"

// AuthoritySubject contains only the metadata needed for control-plane fencing.
type AuthoritySubject struct {
	AccountID  string `json:"account_id"`
	EndpointID string `json:"endpoint_id"`
	Generation uint64 `json:"generation"`
}

func (s *Server) AuthoritySubjects() []AuthoritySubject {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuthoritySubject, 0, len(s.fences))
	for _, f := range s.fences {
		g := f.Grant
		if g.ExpiresAt > time.Now().Unix() && g.AccountID != "" {
			out = append(out, AuthoritySubject{g.AccountID, g.EndpointID, g.Generation})
		}
	}
	return out
}
func (s *Server) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listener != nil && !s.draining
}

// SetConnectionLimit applies approved registry capacity before any listener starts.
func (s *Server) SetConnectionLimit(limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 || limit > MaxConnections || s.listener != nil || len(s.connections)+len(s.wssConnections) != 0 {
		return ErrProtocol
	}
	s.connectionLimit = limit
	return nil
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

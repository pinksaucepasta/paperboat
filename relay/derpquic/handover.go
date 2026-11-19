package derpquic

import (
	"slices"
	"strings"
	"time"
)

// Handover is an account whose relay pool no longer includes this relay: its carriers stay up until CloseAt, so
// clients move to the relays of their new configuration first (make-before-break), then they are closed with the
// non-fatal ErrOverload and refused until the control plane stops listing the account.
type Handover struct {
	AccountID string
	CloseAt   time.Time
}

// SetPoolHandover replaces the relay's handover list: a list entry whose CloseAt has passed closes the account's
// live carriers now and refuses its admissions; a later one waits for a timer. It fences nothing: a grant stays valid
// and works again once the account is no longer listed; an empty list clears every handover.
func (s *Server) SetPoolHandover(list []Handover) error {
	if len(list) > MaxConnections {
		return ErrProtocol
	}
	handover := make(map[string]time.Time, len(list))
	for _, h := range list {
		if h.AccountID == "" || len(h.AccountID) > 256 {
			return ErrProtocol
		}
		handover[h.AccountID] = h.CloseAt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handover = handover
	s.closeHandedOverLocked(time.Now())
	return nil
}

// handedOver reports whether the account's handover deadline has passed (s.mu held).
func (s *Server) handedOver(account string, now time.Time) bool {
	at, ok := s.handover[account]
	return ok && !now.Before(at)
}

// closeHandedOverLocked closes the carriers of accounts past their deadline and arms one timer for the next
// deadline (s.mu held).
func (s *Server) closeHandedOverLocked(now time.Time) {
	var next time.Time
	for _, at := range s.handover {
		if now.Before(at) && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	for k, p := range s.peers {
		if s.handedOver(p.grant.AccountID, now) {
			delete(s.peers, k)
			p.close(ErrOverload.Code, ErrOverload.Message)
		}
	}
	if s.handoverTimer != nil {
		s.handoverTimer.Stop()
		s.handoverTimer = nil
	}
	if !next.IsZero() {
		s.handoverTimer = time.AfterFunc(next.Sub(now), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.closeHandedOverLocked(time.Now())
		})
	}
}

// PoolHandover lists the accounts being handed over, by account.
func (s *Server) PoolHandover() []Handover {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]Handover, 0, len(s.handover))
	for account, at := range s.handover {
		list = append(list, Handover{AccountID: account, CloseAt: at})
	}
	slices.SortFunc(list, func(a, b Handover) int { return strings.Compare(a.AccountID, b.AccountID) })
	return list
}

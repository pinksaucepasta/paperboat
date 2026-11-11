package clientupdate

import "time"

const (
	policyRefreshInterval = 5 * time.Second
	policyReadTimeout     = 2 * time.Second
)

// rememberPolicy requires cacheMu. A delayed read or an idempotent replay must
// never roll the gate back past a newer committed policy.
func (s *Service) rememberPolicy(p Policy) {
	if s.cached != nil && p.Revision < s.cached.Revision {
		return
	}
	value := clonePolicy(p)
	s.cached = &value
	s.checkedAt = time.Now()
}

func clonePolicy(p Policy) Policy {
	if p.EnforceAt != nil {
		at := *p.EnforceAt
		p.EnforceAt = &at
	}
	return p
}

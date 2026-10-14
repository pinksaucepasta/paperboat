package usage

import "time"

type quotaObserver struct {
	authority  string
	admittedAt time.Time
	cancel     func()
}

// SubscribeQuota holds one observer per live ingress lease; closing that lease
// removes it. Existing ingress connection bounds also bound these observers.
func (m *Meter) SubscribeQuota(authority string, admittedAt time.Time, cancel func()) func() {
	return m.Queue.SubscribeQuota(authority, admittedAt, cancel)
}
func (q *Queue) SubscribeQuota(authority string, admittedAt time.Time, cancel func()) func() {
	observer := &quotaObserver{authority, admittedAt, cancel}
	q.mu.Lock()
	if q.quotaObservers == nil {
		q.quotaObservers = make(map[*quotaObserver]struct{})
	}
	q.quotaObservers[observer] = struct{}{}
	q.mu.Unlock()
	return func() { q.mu.Lock(); delete(q.quotaObservers, observer); q.mu.Unlock() }
}

// ExhaustAuthority applies an acknowledged quota result only to streams using
// that immutable authority when the reported usage occurred. A delayed receipt
// must not close newly authorized streams after allowance recovery.
func (q *Queue) ExhaustAuthority(authority string, observedAt time.Time) {
	q.mu.Lock()
	var callbacks []func()
	for observer := range q.quotaObservers {
		if observer.authority == authority && !observer.admittedAt.After(observedAt) {
			callbacks = append(callbacks, observer.cancel)
		}
	}
	q.mu.Unlock()
	for _, cancel := range callbacks {
		cancel()
	}
}

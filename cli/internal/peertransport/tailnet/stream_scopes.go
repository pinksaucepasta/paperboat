package tailnet

// UseStreamScopeFences transfers per-resource revocation to the native stream
// owner before any UDP flow starts. Peer identity and authority expiry remain
// enforced by this owner. Raw UDP callers retain conservative peer-wide fences.
func (a *Authority) UseStreamScopeFences() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.server != nil || a.clientEngine != nil {
		return ErrAuthority
	}
	a.streamScopes = true
	if a.scopeChanges == nil {
		a.scopeChanges = make(chan struct{})
	}
	return nil
}

// ScopeChanges wakes resource owners after a signed update or scope expiry.
// Capture the channel before checking Allows, so no revocation can be missed.
func (a *Authority) ScopeChanges() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scopeChanges == nil {
		a.scopeChanges = make(chan struct{})
	}
	return a.scopeChanges
}

func (a *Authority) notifyScopeChangesLocked() {
	if a.scopeChanges != nil {
		close(a.scopeChanges)
	}
	a.scopeChanges = make(chan struct{})
}

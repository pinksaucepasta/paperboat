package config

// LockLocalAccess serializes trust provisioning, configuration publication and
// daemon activation for this user's browser settings across CLI processes.
func (c *Config) LockLocalAccess() (func() error, error) {
	lock := newSharedLock(c.Path() + ".local-access.lock")
	if err := lock.Lock(); err != nil {
		return nil, err
	}
	return lock.Unlock, nil
}

package selfhost

// ReadConfig returns the existing installation settings under the state owner's
// lock. It never creates an installation or exposes its claims or private keys.
func ReadConfig(dir string) (Config, error) {
	lock, err := lockState(dir)
	if err != nil {
		return Config{}, err
	}
	defer lock.Close()
	existing, err := load(dir)
	if err != nil {
		return Config{}, err
	}
	return existing.Config, nil
}

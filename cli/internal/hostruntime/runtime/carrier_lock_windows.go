package runtime

// Windows runs one carrier generation; generation slots are Linux and macOS only.
func lockCarrier(string) (func() error, error) { return nil, ErrProductionInvalid }

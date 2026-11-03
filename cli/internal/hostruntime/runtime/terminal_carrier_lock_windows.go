package runtime

// Windows keeps the existing reconnect path; native handover is implemented
// for the Linux and macOS launch platforms.
func lockTerminalCarrier(string) (func() error, error) { return nil, ErrProductionInvalid }

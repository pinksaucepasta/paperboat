//go:build windows

package localdaemon

// ServiceProfile is always "" on Windows: the managed service owns its configuration (it takes no --config) and a
// command from any profile only starts it, so no profile can rewrite it.
func ServiceProfile(string) (string, error) { return "", nil }

// OtherProfileError exists on every platform so callers can name it; the Windows service is never replaced by a profile.
type OtherProfileError struct{ Existing, Requested string }

func (e *OtherProfileError) Error() string {
	return "the local daemon service runs for another profile"
}

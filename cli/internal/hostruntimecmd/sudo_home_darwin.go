//go:build darwin

package hostruntimecmd

// sudoHome keeps the root helper out of the user's home: macOS sudo keeps HOME, which sends the helper's state
// paths into a directory it does not own.
func sudoHome() []string { return []string{"-H"} }

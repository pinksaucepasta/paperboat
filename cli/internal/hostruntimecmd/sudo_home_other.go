//go:build !darwin

package hostruntimecmd

// sudoHome is empty off macOS: Linux sudo resets HOME itself, and its launchers stay as they were.
func sudoHome() []string { return nil }

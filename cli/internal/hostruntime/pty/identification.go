package pty

// Identification is a best-effort snapshot of the terminal's foreground job.
// Empty fields mean the operating system cannot currently identify that field.
// It never includes command arguments or environment values.
type Identification struct {
	ForegroundProcess string
	CurrentDirectory  string
}

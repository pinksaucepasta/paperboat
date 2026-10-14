package runtimeport

// The installed runtime listens only on loopback. Installation uses the
// secondary address only when the primary is already occupied.
const (
	Primary   = "127.0.0.1:38080"
	Secondary = "127.0.0.1:48080"
)

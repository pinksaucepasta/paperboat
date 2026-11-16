package sessionname

import "regexp"

// OfflinePrefix starts the id of a terminal session the host admitted while the client could not reach Paperboat.
const OfflinePrefix = "terminal_offline_"

var offlineID = regexp.MustCompile(`^` + OfflinePrefix + `[A-Za-z0-9_-]{1,100}$`)

// IsOfflineID reports whether ref is the id of a session opened without Paperboat.
func IsOfflineID(ref string) bool { return offlineID.MatchString(ref) }

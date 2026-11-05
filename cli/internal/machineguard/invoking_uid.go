package machineguard

import (
	"os"
	"strconv"
)

// InvokingUID is the OS user a Paperboat step acts for: an unprivileged process acts only for itself; a root
// step acts for the explicit PAPERBOAT_INVOKING_UID that Paperboat sets when it re-runs itself through sudo,
// then for SUDO_UID. An inherited SUDO_UID never decides for an unprivileged process (a second user's install
// once acted on the first user's launchd domain that way).
func InvokingUID() (int, bool) { return invokingUID(os.Geteuid(), os.Getuid(), os.Getenv) }

func invokingUID(euid, uid int, getenv func(string) string) (int, bool) {
	if euid != 0 {
		return uid, true
	}
	for _, name := range []string{"PAPERBOAT_INVOKING_UID", "SUDO_UID"} {
		if value := getenv(name); value != "" {
			parsed, err := strconv.Atoi(value)
			return parsed, err == nil && parsed >= 0
		}
	}
	return 0, false
}

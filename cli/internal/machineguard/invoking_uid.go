package machineguard

import (
	"os"
	"strconv"
)

// InvokingUID is the OS user a Paperboat step acts for: a process acts for itself, except a root step that
// Paperboat re-ran through sudo, which acts for the explicit PAPERBOAT_INVOKING_UID it set. SUDO_UID never decides:
// `sudo -i` and an admin's own `sudo pb ...` keep it, and acting for that user split one root install across root
// and the caller (root's daemon read the caller's config and root's pb linked to the caller's binary).
func InvokingUID() (int, bool) { return invokingUID(os.Geteuid(), os.Getuid(), os.Getenv) }

func invokingUID(euid, uid int, getenv func(string) string) (int, bool) {
	if value := getenv("PAPERBOAT_INVOKING_UID"); euid == 0 && value != "" {
		parsed, err := strconv.Atoi(value)
		return parsed, err == nil && parsed >= 0
	}
	return uid, true
}

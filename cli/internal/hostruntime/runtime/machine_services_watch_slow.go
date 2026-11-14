//go:build darwin || windows

package runtime

import "time"

// macOS lists listeners by running lsof twice per pass and Windows walks the TCP table with a process lookup per
// listener: ~0.6% (macOS) and ~0.4% (Windows) of a core at 2 s on an idle machine (PR-03). Tailscale's
// portlist_macos.go polls its netstat every 5 s for the same reason; Linux uses sock_diag and keeps 2 s.
const machineServicesWatchInterval = 5 * time.Second

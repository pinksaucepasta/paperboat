//go:build darwin

package runtime

import "time"

// macOS lists listeners by running lsof twice per pass, which cost the worker ~0.6% of a core at 2 s on an idle machine
// (PR-03, macpool-4); Tailscale's portlist_macos.go polls its netstat every 5 s for the same reason.
const machineServicesWatchInterval = 5 * time.Second

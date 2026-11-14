package runtime

import "time"

// macOS lists listeners by running lsof twice per pass: the worker still used 0.43% of a core at 5 s on an idle
// machine (0.6% at 2 s, PR-03, macpool). Tailscale's portlist_macos.go polls its netstat every 5 s for the same reason.
const machineServicesWatchInterval = 10 * time.Second

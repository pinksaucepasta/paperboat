package runtime

import "time"

// Windows walks the TCP table with a process lookup per listener: ~0.4% of a core at 2 s on an idle machine (PR-03).
// Linux uses sock_diag and keeps 2 s.
const machineServicesWatchInterval = 5 * time.Second

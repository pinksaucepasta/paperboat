package runtime

import "time"

// Windows walks the TCP table with a process lookup per listener: ~0.4% of a core at 2 s on an idle machine (PR-03).
// Linux uses sock_diag and keeps 2 s; macOS 10 s. 15 s here: a just-started service waits at most that long, and a
// browser request for it nudges the services refresh early.
const machineServicesWatchInterval = 15 * time.Second

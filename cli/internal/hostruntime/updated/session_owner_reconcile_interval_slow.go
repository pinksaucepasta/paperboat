//go:build windows || darwin

package updated

import "time"

// A Windows pass (service definitions, DACLs, SCM and pin checks) costs ~30 ms of CPU: at 5 s it was 0.63% of a core
// on an idle machine against 0.08% on Linux (PR-03, winpool-1); on macOS the pass runs launchctl per generation
// (0.31%, macpool-5). It only retires generations an update left behind.
const sessionOwnerReconcileInterval = 15 * time.Second

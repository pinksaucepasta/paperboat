package updated

import "time"

// The retirement/carrier pass only retires generations an update left behind, so it runs once a minute everywhere: at 5 s it
// cost 0.28% of a core on an idle Linux machine, 0.63% on Windows (70 ms per pass in file-attribute and SCM calls, pprof)
// and 0.31% on macOS (launchctl per generation) (PR-03).
const sessionOwnerReconcileInterval = time.Minute

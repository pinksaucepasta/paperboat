//go:build !darwin && !windows

package runtime

import "time"

// machineServicesWatchInterval bounds how long a newly listening port waits before it is announced:
// the periodic observation alone would make other computers wait for the next one.
const machineServicesWatchInterval = 2 * time.Second

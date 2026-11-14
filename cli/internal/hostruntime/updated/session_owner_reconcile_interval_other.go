//go:build !windows

package updated

import "time"

const sessionOwnerReconcileInterval = 5 * time.Second

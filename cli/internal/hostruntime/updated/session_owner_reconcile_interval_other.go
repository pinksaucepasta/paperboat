//go:build !windows && !darwin

package updated

import "time"

const sessionOwnerReconcileInterval = 5 * time.Second

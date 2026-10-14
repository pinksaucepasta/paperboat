//go:build windows

package updated

import ()

// Real PE bytes and protected temporary files exercise the startup identity
// gate. No SCM service, enrolled installation, listener or callback is replaced.

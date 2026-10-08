package processlifetime

import "errors"

var ErrParentUnavailable = errors.New("parent process is unavailable")

var errUnexpectedParentWatchEvent = errors.New("parent exit watch returned an unexpected event")

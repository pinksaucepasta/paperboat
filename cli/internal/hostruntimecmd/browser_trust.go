package hostruntimecmd

import "fmt"

// BrowserTrustPendingError describes a committed installation whose optional
// browser HTTPS trust still requires foreground administrator approval.
type BrowserTrustPendingError struct {
	Cause    error
	Recovery string
}

func (e *BrowserTrustPendingError) Error() string {
	return fmt.Sprintf("Paperboat is installed and its local service is ready; browser HTTPS trust is pending: %v. Recovery: %s", e.Cause, e.Recovery)
}

func (e *BrowserTrustPendingError) Unwrap() error { return e.Cause }

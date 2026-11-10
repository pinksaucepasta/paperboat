package telemetry

import "sync"

// ErrorPage is one error page an edge answered: the reference it showed and its HTTP status.
type ErrorPage struct {
	Reference string
	Status    int
}

// maxPendingErrorPages bounds the references waiting for the next heartbeat. Past it a page still shows its reference
// and the edge log still carries it; only the server's support_reports row is skipped.
const maxPendingErrorPages = 64

var pendingErrorPages struct {
	mu    sync.Mutex
	pages []ErrorPage
}

// NoteErrorPage remembers a reference an error page showed a visitor, for TakeErrorPages.
func NoteErrorPage(reference string, status int) {
	pendingErrorPages.mu.Lock()
	defer pendingErrorPages.mu.Unlock()
	if len(pendingErrorPages.pages) < maxPendingErrorPages {
		pendingErrorPages.pages = append(pendingErrorPages.pages, ErrorPage{Reference: reference, Status: status})
	}
}

// TakeErrorPages returns and forgets the references noted since the last call (the heartbeat sends them).
func TakeErrorPages() []ErrorPage {
	pendingErrorPages.mu.Lock()
	defer pendingErrorPages.mu.Unlock()
	pages := pendingErrorPages.pages
	pendingErrorPages.pages = nil
	return pages
}

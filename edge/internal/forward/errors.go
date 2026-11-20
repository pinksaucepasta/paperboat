package forward

import "errors"

var (
	// ErrNoHolder: no node is known to serve the key.
	ErrNoHolder = errors.New("no node serves this name")
	// ErrUnavailable: nodes serve the key but none could be reached or would take the visitor. It is the typed
	// cause behind the visitor's "could not reach the node serving this address" page.
	ErrUnavailable = errors.New("no node serving this name could be reached")
	// ErrDraining is what an Injector answers once its HTTP server can no longer accept visitors.
	ErrDraining = errors.New("node is draining and takes no new forwarded visitors")
	// ErrBusy is what an Injector answers when it has no room for another forwarded visitor.
	ErrBusy = errors.New("node has no room for another forwarded visitor")
	// ErrStopped: the mesh is not running.
	ErrStopped = errors.New("node mesh is not running")

	errUnknownPeer = errors.New("forward peer is not in the directory")
)

// Failure is a forward that failed before any byte reached the visitor, with the stage that failed
// (forward_lookup, forward_dial or forward_open: the reportcause stages).
type Failure struct {
	Stage string
	Err   error
}

func (f *Failure) Error() string   { return "forward " + f.Stage + ": " + f.Err.Error() }
func (f *Failure) Unwrap() []error { return []error{ErrUnavailable, f.Err} }

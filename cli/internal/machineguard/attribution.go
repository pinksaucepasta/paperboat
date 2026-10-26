package machineguard

import (
	"errors"
	"net"
	"net/netip"
	"time"
)

// Gateway attribution (PLAN 3b multi-account hosts, rev 3): the host gateway hands a connection to the daemon of the
// OS user that owns the CONNECTING socket, found from the kernel by the exact tuple. It fails closed: no single live
// owner, or an owner without a registered daemon, refuses the connection. It never uses the accepted socket's owner,
// a hostname, a cached tuple or "the active user".

// ErrAttribution is the class of every attribution refusal; callers branch on the typed causes below.
var ErrAttribution = errors.New("gateway connection owner could not be attributed")

type attributionError struct{ reason string }

func (e attributionError) Error() string      { return "gateway attribution: " + e.reason }
func (attributionError) Is(target error) bool { return target == ErrAttribution }

var (
	errAttributionNotFound     = attributionError{"no live connecting socket for the connection"}
	errAttributionAmbiguous    = attributionError{"more than one connecting socket matches the connection"}
	errAttributionInconsistent = attributionError{"the kernel answered for a different connection"}
	errAttributionUnsupported  = attributionError{"only IPv4 loopback TCP connections are attributed"}
	errAttributionNoDaemon     = attributionError{"the connecting OS user has no Paperboat daemon on this computer"}
)

const (
	attributionAttempts = 3
	attributionBudget   = 50 * time.Millisecond
)

// socketTuple is the connecting (client) socket: Source is the client's address, Destination the gateway's.
type socketTuple struct {
	Source, Destination netip.AddrPort
}

// socketOwner is one kernel answer for a tuple.
type socketOwner struct {
	Tuple       socketTuple
	UID         uint32
	Established bool
}

// lookupOwners asks the kernel which sockets have tuple. It returns every match.
type lookupOwners func(socketTuple) ([]socketOwner, error)

// clientTuple is the connecting socket's tuple of a connection the gateway accepted: the reverse of the accepted one.
func clientTuple(accepted net.Conn) (socketTuple, error) {
	local, lok := accepted.LocalAddr().(*net.TCPAddr)
	remote, rok := accepted.RemoteAddr().(*net.TCPAddr)
	if !lok || !rok {
		return socketTuple{}, errAttributionUnsupported
	}
	tuple := socketTuple{Source: remote.AddrPort(), Destination: local.AddrPort()}
	tuple.Source = netip.AddrPortFrom(tuple.Source.Addr().Unmap(), tuple.Source.Port())
	tuple.Destination = netip.AddrPortFrom(tuple.Destination.Addr().Unmap(), tuple.Destination.Port())
	if !tuple.Source.Addr().Is4() || !tuple.Source.Addr().IsLoopback() || !tuple.Destination.Addr().IsLoopback() {
		return socketTuple{}, errAttributionUnsupported
	}
	return tuple, nil
}

// attributeOwner returns the uid of the single live connecting socket of tuple. A missing answer is retried, at most
// attributionAttempts times within attributionBudget; an ambiguous, inconsistent or not established answer is refused
// at once.
func attributeOwner(tuple socketTuple, lookup lookupOwners) (uint32, error) {
	deadline := time.Now().Add(attributionBudget)
	err := error(errAttributionNotFound)
	for attempt := 0; attempt < attributionAttempts; attempt++ {
		if attempt > 0 {
			wait := time.Until(deadline) / time.Duration(attributionAttempts-attempt)
			if wait <= 0 {
				break
			}
			time.Sleep(wait)
		}
		owners, lookupErr := lookup(tuple)
		switch {
		case lookupErr != nil:
			err = errors.Join(errAttributionNotFound, lookupErr)
			continue
		case len(owners) == 0:
			err = errAttributionNotFound
			continue
		case len(owners) > 1:
			return 0, errAttributionAmbiguous
		case owners[0].Tuple != tuple:
			return 0, errAttributionInconsistent
		case !owners[0].Established:
			// A closing or TIME_WAIT socket no longer names its user (time-wait sockets report uid 0).
			return 0, errAttributionNotFound
		}
		return owners[0].UID, nil
	}
	return 0, err
}

// attributeConnection finds the daemon an accepted gateway connection belongs to. daemons maps a uid to its
// registered daemon; a uid without one (root, system users, users who never installed) is refused with
// errAttributionNoDaemon, which the gateway answers with the refusal page on 80/443 and a close elsewhere.
func attributeConnection[D any](accepted net.Conn, lookup lookupOwners, daemons func(uint32) (D, bool)) (uint32, D, error) {
	var none D
	tuple, err := clientTuple(accepted)
	if err != nil {
		return 0, none, err
	}
	uid, err := attributeOwner(tuple, lookup)
	if err != nil {
		return 0, none, err
	}
	daemon, ok := daemons(uid)
	if !ok {
		return uid, none, errAttributionNoDaemon
	}
	return uid, daemon, nil
}

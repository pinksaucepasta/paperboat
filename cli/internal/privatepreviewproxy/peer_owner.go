package privatepreviewproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
)

// The proxy and its PAC are one OS user's: a loopback port is open to every local user, so each connection must
// come from the user that runs this hostd. The owner is found from the kernel by the connection's tuple, with the
// guard's attribution (machineguard.ConnectionOwnerUID); anything else is refused and counted by reason.

const (
	refusalOtherUser    = "other_user"
	refusalLookupFailed = "lookup_failed"
)

// platformPeerOwner is nil where the platform has no attribution (see peer_other.go).
var platformPeerOwner func(net.Conn) (uint32, error)

type peerCheck struct {
	conn net.Conn
	once sync.Once
	err  error
}

type peerCheckKey struct{}

func peerConnContext(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, peerCheckKey{}, &peerCheck{conn: conn})
}

// refusals counts refused connections by a fixed set of reasons.
type refusals struct {
	mu     sync.Mutex
	counts map[string]uint64
}

func (r *refusals) add(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[string]uint64{}
	}
	r.counts[reason]++
}

// Refusals reports the connections refused so far, by reason.
func (p *AccessProxy) Refusals() map[string]uint64 {
	p.refused.mu.Lock()
	defer p.refused.mu.Unlock()
	out := make(map[string]uint64, len(p.refused.counts))
	for reason, n := range p.refused.counts {
		out[reason] = n
	}
	return out
}

// peerAllowed decides once per connection whether its peer is the owning user.
func (p *AccessProxy) peerAllowed(request *http.Request) bool {
	if p.peerOwner == nil {
		return true
	}
	check, _ := request.Context().Value(peerCheckKey{}).(*peerCheck)
	if check == nil {
		return false
	}
	check.once.Do(func() {
		uid, err := p.peerOwner(check.conn)
		switch {
		case err != nil:
			p.refused.add(peerRefusalReason(err))
			check.err = err
		case uid != p.ownerUID:
			p.refused.add(refusalOtherUser)
			check.err = errOtherUser
		}
	})
	return check.err == nil
}

var errOtherUser = errors.New("connection is not from the owning OS user")

func defaultPeerOwner() (func(net.Conn) (uint32, error), uint32) {
	return platformPeerOwner, uint32(os.Geteuid())
}

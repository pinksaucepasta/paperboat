package privatepreviewproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
)

// The proxies are one OS user's: a loopback port is open to every local user, and each connection opens a
// stream under hostd's machine session. So every connection must come from the user that runs this hostd. The
// owner is found from the kernel by the connection's tuple, with the guard's attribution
// (machineguard.ConnectionOwnerUID); anything else is refused and counted by reason.

const (
	refusalOtherUser    = "other_user"
	refusalLookupFailed = "lookup_failed"
)

// platformPeerOwner (a uid) or platformPeerSID (Windows) is nil where the platform has no attribution (see peer_other.go).
var (
	platformPeerOwner func(net.Conn) (uint32, error)
	platformPeerSID   func(net.Conn) (string, error)
)

var errOtherUser = errors.New("connection is not from the owning OS user")

type peerGate struct {
	owner    func(net.Conn) (uint32, error)
	uid      uint32
	ownerSID func(net.Conn) (string, error) // Windows: the owner is a SID, which a uint32 cannot hold
	sid      string
	mu       sync.Mutex
	refused  map[string]uint64
}

func newPeerGate() *peerGate {
	return &peerGate{owner: platformPeerOwner, uid: uint32(os.Geteuid()), ownerSID: platformPeerSID, sid: platformOwnSID()}
}

func (g *peerGate) enforced() bool { return g.owner != nil || g.ownerSID != nil }

// allow reports whether conn's peer is the owning user, counting a refusal by reason.
func (g *peerGate) allow(conn net.Conn) bool {
	if !g.enforced() {
		return true
	}
	var err error
	other := false
	if g.ownerSID != nil {
		var sid string
		sid, err = g.ownerSID(conn)
		other = sid != g.sid
	} else {
		var uid uint32
		uid, err = g.owner(conn)
		other = uid != g.uid
	}
	reason := ""
	switch {
	case err != nil:
		reason = peerRefusalReason(err)
	case other:
		reason = refusalOtherUser
	default:
		return true
	}
	g.mu.Lock()
	if g.refused == nil {
		g.refused = map[string]uint64{}
	}
	g.refused[reason]++
	g.mu.Unlock()
	return false
}

func (g *peerGate) refusals() map[string]uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]uint64, len(g.refused))
	for reason, n := range g.refused {
		out[reason] = n
	}
	return out
}

// Refusals reports the connections refused so far, by reason.
func (p *AccessProxy) Refusals() map[string]uint64 { return p.gate.refusals() }

// Refusals reports the connections refused so far, by reason.
func (p *Proxy) Refusals() map[string]uint64 { return p.gate.refusals() }

type peerCheck struct {
	conn    net.Conn
	once    sync.Once
	allowed bool
}

type peerCheckKey struct{}

func peerConnContext(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, peerCheckKey{}, &peerCheck{conn: conn})
}

// peerAllowed decides once per HTTP connection whether its peer is the owning user.
func (p *AccessProxy) peerAllowed(request *http.Request) bool {
	check, _ := request.Context().Value(peerCheckKey{}).(*peerCheck)
	if check == nil {
		return !p.gate.enforced()
	}
	check.once.Do(func() { check.allowed = p.gate.allow(check.conn) })
	return check.allowed
}

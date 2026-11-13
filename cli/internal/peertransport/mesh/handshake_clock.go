package mesh

import (
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/tailscale/wireguard-go/tai64n"
)

// A WireGuard responder drops an initiation whose timestamp is not newer than the last it accepted from that
// peer (wireguard-go device/noise-protocol.go ConsumeMessageInitiation). Stamped on the raw local clock, a
// machine whose clock ran ahead and was set back (X-27) was refused by every peer it had handshaken with
// until real time passed the old stamp, about as long as the skew. Handshakes are stamped on the server
// clock, which does not follow a local clock step, and never behind the previous stamp of this process.
var handshakeBase = serverclock.Now

var handshakeStamp struct {
	sync.Mutex
	last time.Time
}

func handshakeTime() time.Time {
	handshakeStamp.Lock()
	defer handshakeStamp.Unlock()
	t := handshakeBase()
	if !t.After(handshakeStamp.last) {
		t = handshakeStamp.last.Add(time.Second)
	}
	handshakeStamp.last = t
	return t
}

func init() { tai64n.Clock = handshakeTime }

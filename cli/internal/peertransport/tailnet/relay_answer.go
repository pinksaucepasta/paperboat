package tailnet

import (
	"context"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/backoff"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=regional-recovery
	"tailscale.com/net/stun"
)

// relayAnswerInterval spaces the STUN requests that look for a working network while renewals fail.
const relayAnswerInterval = 2 * time.Second

// startRelayProbe looks for a relay answer until stopped; the first one asks Run for one renewal now.
func (a *Authority) startRelayProbe(ctx context.Context) (stop func()) {
	probe, stop := context.WithCancel(ctx)
	go func() {
		if a.awaitRelayAnswer(probe) {
			select {
			case a.relayAnswered <- struct{}{}:
			default:
			}
		}
	}()
	return stop
}

// awaitRelayAnswer sends a STUN binding request to every relay of the last candidate set (stale ones too: only the
// address is used) every relayAnswerInterval until one answers, then reports true: packets flow again. A network that
// drops everything and returns, or a NAT that rebinds, changes no local link, so nothing else says so; Tailscale learns
// it the same way, from a STUN answer or a DERP reconnect (wgengine/magicsock/magicsock.go ReSTUN, derp.go
// runDerpReader). Relays answer STUN statelessly; Paperboat's API is never asked.
func (a *Authority) awaitRelayAnswer(ctx context.Context) bool {
	socket, err := net.ListenUDP("udp", nil)
	if err != nil {
		return false
	}
	defer socket.Close()
	answered := make(chan struct{}, 1)
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, _, err := socket.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			if _, _, err := stun.ParseResponse(buffer[:n]); err == nil {
				select {
				case answered <- struct{}{}:
				default:
				}
				return
			}
		}
	}()
	for {
		a.mu.Lock()
		var relays []RegionalNode
		if a.regional != nil {
			relays = append(relays, a.regional.Nodes...)
		}
		a.mu.Unlock()
		for _, n := range relays {
			if n.EndpointHost == "" || n.EndpointTCPPort == 0 {
				continue
			}
			// A resolver of its own: the shared one would join a lookup sent into the outage.
			lookup, cancel := context.WithTimeout(ctx, time.Second)
			ips, err := (&net.Resolver{}).LookupNetIP(lookup, "ip", n.EndpointHost)
			cancel()
			if err == nil && len(ips) > 0 {
				_, _ = socket.WriteToUDP(stun.Request(stun.NewTxID()), &net.UDPAddr{IP: ips[0].AsSlice(), Port: int(n.EndpointTCPPort)})
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-answered:
			return true
		case <-time.After(backoff.Jitter(relayAnswerInterval)):
		}
	}
}

package runtimeport

import (
	"crypto/sha256"
	"encoding/binary"
	"net"
	"strconv"
)

// The installed runtime listens only on loopback, on a port of its own per OS user: a host serves any number of
// users. The account's seed (the Windows SID, the Unix uid) picks the first candidate and the following ports come
// next, wrapping inside [First, First+Span); the installer takes the first free one and keeps it.
const (
	First    = 38080
	Span     = 10000
	Attempts = 256
)

// Primary is the address an installation that is not enrolled yet carries until its real one is chosen.
const Primary = "127.0.0.1:38080"

// Candidates are the loopback addresses to try for one account, in order.
func Candidates(seed string) []string {
	digest := sha256.Sum256([]byte(seed))
	start := int(binary.BigEndian.Uint32(digest[:4]) % Span)
	addresses := make([]string, Attempts)
	for i := range addresses {
		addresses[i] = net.JoinHostPort("127.0.0.1", strconv.Itoa(First+(start+i)%Span))
	}
	return addresses
}

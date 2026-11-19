// Package terminalprotocol owns terminal transport negotiation names shared by hosts, clients and edges.
package terminalprotocol

const (
	TerminalSubprotocol = "paperboat.terminal.v1"
	BrowserSubprotocol  = "paperboat.browser-terminal.e2ee.v1"
	BrowserTicketPrefix = "pb-ticket."
)

// BrowserFileEnvelope is the first byte of a browser-to-edge WebSocket message whose TLS record carries a FILE transfer request, in
// place of 0x00 (a TLS record of anything else). The edge cannot read the end-to-end encrypted stream, so this marker, set by the
// browser's writer, is how it counts and paces uploads apart from typing. The edge strips it; the machine never sees it.
//
// Prior art: ttyd's WebSocket carries a command byte in the clear (INPUT '0', RESIZE_TERMINAL '1': ttyd/src/server.h) because it has no
// end-to-end encryption, and its ZMODEM file transfer rides inside the PTY stream where its server cannot tell it from output, which is
// the case this marker solves; Tailscale keeps Taildrop on its own PeerAPI endpoint (feature/taildrop/peerapi.go, PUT /v0/put/), apart from
// SSH, so a node can tell file traffic from sessions without reading either. A separate encrypted connection for uploads would be that
// design here; the marker gives the same visibility on the one connection without a second ticket and handshake.
const BrowserFileEnvelope byte = 0x03

// Package peernetwork holds the bounds of the signed peer network configuration, shared by the server
// that issues it and every client that verifies it, so a configuration the server may send is always
// one the client accepts.
package peernetwork

const (
	// MaxEndpoints is the stated product maximum of endpoints (computers and signed-in CLI sessions)
	// one configuration names: 1024 computers with their pairing sessions. Plans do not limit machines.
	MaxEndpoints = 2048
	// MaxScopes bounds the scopes of all peers of one configuration.
	MaxScopes = 2 * MaxEndpoints
	// MaxPeerBytes bounds the signed encoding of one peer with its scopes; a peer with two scopes
	// encodes to about 1.3 KiB.
	MaxPeerBytes = 2 << 10
	// MaxConfigurationBytes bounds one signed configuration (and relay grant or regional candidate
	// token): every endpoint at MaxPeerBytes plus the configuration's own fields.
	MaxConfigurationBytes = MaxEndpoints*MaxPeerBytes + 16<<10
)

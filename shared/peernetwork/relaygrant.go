package peernetwork

// RelayGrant is the signed claim set of a relay grant (JWT type "paperboat-relay-grant+jwt"). The server builds it,
// mint signs it, and the relay decodes it strictly: a signed authorization claim it does not know could be a
// restriction, so it refuses the grant. Because all three use this one type, the server cannot emit a claim the
// relay lacks.
type RelayGrant struct {
	Version                int              `json:"version"`
	Issuer                 string           `json:"iss"`
	Audience               string           `json:"aud"`
	IssuedAt               int64            `json:"iat"`
	ExpiresAt              int64            `json:"exp"`
	Generation             uint64           `json:"generation"`
	AccountID              string           `json:"account_id"`
	EndpointID             string           `json:"endpoint_id"`
	WireGuardPublicKey     string           `json:"wireguard_public_key"`
	DiscoPublicKey         string           `json:"disco_public_key,omitempty"`
	CertificateFingerprint string           `json:"quic_certificate_fingerprint"`
	QUICPublicKey          string           `json:"quic_public_key"`
	NodeID                 string           `json:"node_id"`
	NodeGeneration         uint64           `json:"node_generation"`
	ProcessEpoch           string           `json:"process_epoch"`
	Peers                  []RelayPeer      `json:"peers"`
	PeerRelay              *RelayDescriptor `json:"peer_relay,omitempty"`
	RelayControlPeers      []string         `json:"relay_control_peers,omitempty"`
	// Memberships authorize pairs without per-peer lists; a grant has one or the other.
	Memberships []Scope `json:"memberships,omitempty"`
}

// RelayPeer is one peer a relay grant pairs its endpoint with.
type RelayPeer struct {
	WireGuardPublicKey string  `json:"wireguard_public_key"`
	DiscoPublicKey     string  `json:"disco_public_key,omitempty"`
	Scopes             []Scope `json:"scopes"`
}

// RelayDescriptor names a peer relay service.
type RelayDescriptor struct {
	WireGuardPublicKey string `json:"wireguard_public_key"`
	DiscoPublicKey     string `json:"disco_public_key"`
	VirtualAddress     string `json:"virtual_address"`
}

// Bounds of one relay grant (part), shared by the server that chunks and mint that signs (both refuse more) and the
// relay that verifies. An endpoint with more peers gets more grants of the same generation.
const (
	MaxGrantPeers  = 64
	MaxGrantScopes = 128 // the scopes of all peers of one grant
	// MaxGrantBytes bounds one signed grant token: the relay reads it from one control frame (derpquic.MaxControl).
	MaxGrantBytes = 128 << 10
)

// RelayGrantVersion and RelayGrantAudience are the values every relay grant carries.
const (
	RelayGrantVersion  = 1
	RelayGrantAudience = "paperboat-relay"
	RelayGrantType     = "paperboat-relay-grant+jwt"
)

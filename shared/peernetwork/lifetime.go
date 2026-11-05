package peernetwork

import "time"

// Lifetime is how long the server signs a peer network configuration, its relay grants and its regional
// candidate set for, and the longest a relay or client accepts. Clients renew at about half of it, on a jittered
// safety refresh, and at once when the server announces a change (tailscale control/controlclient/map.go streams
// the netmap on change; headscale hscontrol/poll.go). A relay verifies a grant when it admits a connection and
// the admitted connection lives until revocation, drain or a network fault (tailscale derp/derpserver).
const Lifetime = 10 * time.Minute

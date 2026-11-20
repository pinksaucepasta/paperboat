package edgehttp

import "github.com/pinksaucepasta/paperboat/shared/addrlimit"

// Edge-wide limits per client address (edge abuse plan, layers L2 and L3): the flood floor. The limiter
// is shared/addrlimit; these are the edge's rates. Prior art: zrok's public frontend limits per client IP;
// ngrok's rate-limit action keyed on conn.client_ip.
var (
	// One address's HTTP requests (any number of links) and new WebSocket upgrades on this edge.
	addressRequestRate = addrlimit.Rate{PerSecond: 100, Burst: 3000}
	addressUpgradeRate = addrlimit.Rate{PerSecond: 20, Burst: 40}
	// One address's TLS handshakes on the shared 443 listener.
	addressHandshakeRate = addrlimit.Rate{PerSecond: 20, Burst: 50}
)

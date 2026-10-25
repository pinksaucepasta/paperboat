// Package edgelimits is the edge's abuse protection on public ingress: how many connections,
// stream opens and bytes one publication and one account may use on one edge process. It is
// one server configuration set, the same for every plan (plan limits live in the plan table),
// sent to every edge in its heartbeat answer. Each edge of an account's pair applies it on its
// own: normal traffic uses only the active edge. Prior art: zrok computes per-account limits
// centrally (controller/limits); Teleport's connection limiter comes from config.
package edgelimits

import "errors"

// ErrInvalid reports a limit set an edge must not apply.
var ErrInvalid = errors.New("edge ingress limits invalid")

// minimumByteBurst is the edge's pacing chunk: a smaller burst could never pass one chunk.
const minimumByteBurst = 32 << 10

// Limit bounds one key (a publication or an account) on one edge process.
type Limit struct {
	Connections    int     `json:"connections"`
	OpensPerSecond float64 `json:"opens_per_second"`
	OpenBurst      int     `json:"open_burst"`
	BytesPerSecond int64   `json:"bytes_per_second"`
	ByteBurst      int     `json:"byte_burst"`
}

// IngressLimits is the whole set an edge applies.
type IngressLimits struct {
	Publication Limit `json:"publication"`
	Account     Limit `json:"account"`
}

// Default is the set used until the server sends one, and the server's own default. One
// publication's byte rate stays above one stream's 150 Mbit/s target (PR-06).
func Default() IngressLimits {
	return IngressLimits{
		Publication: Limit{Connections: 64, OpensPerSecond: 10, OpenBurst: 32, BytesPerSecond: 24 << 20, ByteBurst: 1 << 20},
		Account:     Limit{Connections: 128, OpensPerSecond: 50, OpenBurst: 128, BytesPerSecond: 32 << 20, ByteBurst: 1 << 20},
	}
}

func (l Limit) validate() error {
	if l.Connections < 1 || l.Connections > 1_000_000 || l.OpensPerSecond <= 0 || l.OpensPerSecond > 1_000_000 ||
		l.OpenBurst < 1 || l.OpenBurst > 1_000_000 || l.BytesPerSecond < minimumByteBurst || l.BytesPerSecond > 100<<30 ||
		l.ByteBurst < minimumByteBurst || l.ByteBurst > 1<<30 {
		return ErrInvalid
	}
	return nil
}

// Validate rejects a set with a missing, zero or absurd value, or an account limit below its
// publication's.
func (l IngressLimits) Validate() error {
	if l.Publication.validate() != nil || l.Account.validate() != nil ||
		l.Account.Connections < l.Publication.Connections || l.Account.BytesPerSecond < l.Publication.BytesPerSecond {
		return ErrInvalid
	}
	return nil
}

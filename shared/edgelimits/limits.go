// Package edgelimits is the edge's byte pacing on public ingress: how fast one publication and
// one account may move bytes on one edge process, and the slow rate of a workspace that used its
// fair-use bandwidth. It shapes and never refuses: popularity is not abuse, so request and
// connection counts are not capped here (floods are stopped per client IP at the edge, and the
// plan's fair-use bandwidth is the user-facing limit). It is one server
// configuration set, the same for every plan, sent to every edge in its heartbeat answer. Prior
// art: frp's per-proxy transport.bandwidthLimit; zrok computes per-account limits centrally.
package edgelimits

import "errors"

// ErrInvalid reports a limit set an edge must not apply.
var ErrInvalid = errors.New("edge ingress limits invalid")

// minimumByteBurst is the edge's pacing chunk: a smaller burst could never pass one chunk.
const minimumByteBurst = 32 << 10

// Limit paces one key (a publication or an account) on one edge process.
type Limit struct {
	BytesPerSecond int64 `json:"bytes_per_second"`
	ByteBurst      int   `json:"byte_burst"`
}

// IngressLimits is the whole set an edge applies.
type IngressLimits struct {
	Publication Limit `json:"publication"`
	Account     Limit `json:"account"`
	// Throttled is the rate all routes of one workspace share on one edge after the workspace used its
	// fair-use bandwidth, until its allowance period ends: 1 Mbit/s.
	Throttled Limit `json:"throttled"`
}

// Default is the set used until the server sends one, and the server's own default. One
// publication's byte rate stays above one stream's 150 Mbit/s target (PR-06).
func Default() IngressLimits {
	return IngressLimits{
		Publication: Limit{BytesPerSecond: 24 << 20, ByteBurst: 1 << 20},
		Account:     Limit{BytesPerSecond: 32 << 20, ByteBurst: 1 << 20},
		Throttled:   Limit{BytesPerSecond: 125_000, ByteBurst: minimumByteBurst},
	}
}

func (l Limit) validate() error {
	if l.BytesPerSecond < minimumByteBurst || l.BytesPerSecond > 100<<30 || l.ByteBurst < minimumByteBurst || l.ByteBurst > 1<<30 {
		return ErrInvalid
	}
	return nil
}

// Validate rejects a set with a missing, zero or absurd value, an account rate below its
// publication's, or a throttle rate above the publication's.
func (l IngressLimits) Validate() error {
	if l.Publication.validate() != nil || l.Account.validate() != nil || l.Throttled.validate() != nil || l.Account.BytesPerSecond < l.Publication.BytesPerSecond || l.Throttled.BytesPerSecond > l.Publication.BytesPerSecond {
		return ErrInvalid
	}
	return nil
}

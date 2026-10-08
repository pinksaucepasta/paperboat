package inspector

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalid         = errors.New("invalid inspector request")
	ErrDisabled        = errors.New("inspector capture is not enabled for this resource")
	ErrForbidden       = errors.New("inspector action is not authorized")
	ErrStaleAuthority  = errors.New("inspector authority is stale or revoked")
	ErrStaleGeneration = errors.New("inspector resource generation is stale")
	ErrNotFound        = errors.New("inspector capture is not found")
	ErrExpired         = errors.New("inspector capture has expired")
	ErrTooManyReads    = errors.New("inspector has too many concurrent reads")
	ErrDropped         = errors.New("inspector dropped capture under budget pressure")
)

// Fixed v1 budgets from the preview-tunnel-v1 contract. Byte units are binary.
const (
	DaemonMaxBytes         = 64 << 20
	DaemonMaxRecords       = 2000
	ResourceMaxBytes       = 8 << 20
	ResourceMaxRecords     = 200
	Retention              = 15 * time.Minute
	MetadataMaxBytes       = 16 << 10
	URLMaxBytes            = 2048
	URLInputMaxBytes       = 64 << 10
	JSONMaxDepth           = 64
	MaxHeadersPerDirection = 64
	MaxSensitiveNames      = 64
	BodyMaxBytes           = 64 << 10
	RawMaxBytes            = 256 << 10
	RawRetention           = 2 * time.Minute
	WorkQueueDaemonMax     = 128
	WorkQueueResourceMax   = 16
	RetrievalMaxRecords    = 100
	RetrievalMaxBytes      = 1 << 20
	MaxConcurrentReads     = 4
	SlowReadTimeout        = 10 * time.Second
	AuthorityFreshness     = 10 * time.Second
)

// CaptureState is the lifecycle state of a record or a single body.
type CaptureState string

const (
	StateComplete    CaptureState = "complete"
	StateTruncated   CaptureState = "truncated"
	StateDropped     CaptureState = "dropped"
	StateUnsupported CaptureState = "unsupported"
	StateExpired     CaptureState = "expired"
)

// Action separates ordinary viewing from inspection and replay.
type Action string

const (
	ActionView    Action = "view"
	ActionInspect Action = "inspect"
	ActionReplay  Action = "replay"
)

// Credential is the daemon-local proof presented for retrieval. It must carry
// an inspect action for sanitized reads and a replay action for raw reads; a
// view grant never authorizes inspection. Expected generations bind the request
// to current server resource authority. AuthorityReadAt is the time of the
// authoritative server read backing this credential.
type Credential struct {
	PrincipalID        string
	Action             Action
	ResourceID         string
	ResourceGeneration uint64
	RouteGeneration    uint64
	TargetGeneration   uint64
	AuthorityReadAt    time.Time
	ExpiresAt          time.Time
}

// ResourcePolicy is the explicit per-resource opt-in. Capture stays disabled
// until the owner enables it; body and raw modes are independent opt-ins.
type ResourcePolicy struct {
	Enabled             bool
	CaptureRequestBody  bool
	CaptureResponseBody bool
	CaptureRaw          bool
	// Current generations fence retrieval. Zero means the caller has no
	// generation information; exact nonzero values must match on retrieval.
	ResourceGeneration uint64
	RouteGeneration    uint64
	TargetGeneration   uint64
	Revoked            bool
	// SensitiveNames adds case-insensitive body field names to the built-in
	// password/secret/token/key matcher.
	SensitiveNames []string
}

func validResourceID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// Package offlineaudit is the host-recorded audit event a machine queues while Paperboat is unreachable (or records
// for an SSH/exec session) and uploads under its machine proof; the server validates each kind strictly. Kind,
// reason and outcome vocabularies and the queue bounds stay with their owners.
package offlineaudit

import (
	"time"

	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// Event is one host-recorded offline session start, end or refusal, or a recording lifecycle event.
type Event struct {
	EventID        string    `json:"event_id"`
	Kind           string    `json:"kind"`
	Capability     string    `json:"capability"`
	PeerEndpointID string    `json:"peer_endpoint_id"`
	PeerAccountID  string    `json:"peer_account_id"`
	OperationID    string    `json:"operation_id"`
	OccurredAt     time.Time `json:"occurred_at"`
	Reason         string    `json:"reason,omitempty"`
	// The fields below belong to the ssh_session_* kinds: Origin says whether Paperboat was reachable, SessionID
	// (an end's) is the started event's EventID.
	Origin     string `json:"origin,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	BytesIn    int64  `json:"bytes_in,omitempty"`
	BytesOut   int64  `json:"bytes_out,omitempty"`
	// ExitCode and DurationMS belong to an exec's offline_session_ended.
	ExitCode   *int  `json:"exit_code,omitempty"`
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Recording belongs to the recording_* kinds, which carry nothing else but the event id, kind and time.
	Recording *recording.Report `json:"recording,omitempty"`
}

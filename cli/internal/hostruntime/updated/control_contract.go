//go:build darwin || linux || windows

package updated

import (
	"errors"
	"regexp"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

const ControlProtocolV1 = "paperboat.updated/v1"
const maxUpdateControlTimeout = 15 * time.Minute

var (
	ErrInvalidControl   = errors.New("invalid paperboat-updated control request")
	ErrControlDenied    = errors.New("paperboat-updated control peer is not the enrolled user")
	ErrApprovalRequired = errors.New("download the update and approve its exact candidate before installing")
	ErrCandidateChanged = errors.New("downloaded update changed; review and approve the new candidate")
	exactReleasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	approvalIDPattern   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type ControlRequest struct {
	Schema     string `json:"schema"`
	Operation  string `json:"operation"`
	ApprovalID string `json:"approval_id,omitempty"`
}
type ControlResponse struct {
	UpdaterVersion    string                          `json:"updater_version,omitempty"`
	Schema            string                          `json:"schema"`
	Status            string                          `json:"status"`
	Version           string                          `json:"version,omitempty"`
	Updated           bool                            `json:"updated"`
	Pending           bool                            `json:"pending,omitempty"`
	Candidate         *workerupdate.PreparedCandidate `json:"candidate,omitempty"`
	ActivationFailure string                          `json:"activation_failure,omitempty"`
	Observation       autoupdate.Observation          `json:"observation"`
	ErrorCode         string                          `json:"error_code,omitempty"`
	ErrorMessage      string                          `json:"error_message,omitempty"`
	Transaction       workerupdate.TransactionState   `json:"transaction"`
}

func validControlRequest(request ControlRequest) bool {
	if request.Schema != "" && request.Schema != ControlProtocolV1 {
		return false
	}
	switch request.Operation {
	case "status", "check", "download":
		return request.ApprovalID == ""
	case "install":
		return approvalIDPattern.MatchString(request.ApprovalID)
	default:
		return false
	}
}

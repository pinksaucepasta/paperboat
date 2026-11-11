//go:build darwin || linux || windows

package updated

import (
	"errors"
	"regexp"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

const ControlProtocolV1 = "paperboat.updated/v1"
const maxUpdateControlTimeout = 15 * time.Minute

// controlRequestReadTimeout bounds how long a control connection may take to send its one request line: a client
// that connects and sends nothing must not hold a handler for the 15 minutes an install may take.
var controlRequestReadTimeout = 10 * time.Second

// controlOperationTimeout is the budget of one control operation, by what it does: reading status or settings is
// local (seconds), check resolves release metadata over the network (a few minutes), download and install fetch
// and activate (the full update window). One stuck connection must not hold the control slot for 15 minutes.
func controlOperationTimeout(operation string) time.Duration {
	switch operation {
	case "status", "settings":
		return 30 * time.Second
	case "check":
		return 3 * time.Minute
	}
	return maxUpdateControlTimeout
}

var (
	ErrInvalidControl   = errors.New("invalid paperboat-updated control request")
	ErrControlDenied    = errors.New("paperboat-updated control peer authorization failed")
	ErrApprovalRequired = errors.New("download the update and approve its exact candidate before installing")
	ErrCandidateChanged = errors.New("downloaded update changed; review and approve the new candidate")
	exactReleasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	approvalIDPattern   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type ControlRequest struct {
	Generation string                  `json:"generation,omitempty"`
	Schema     string                  `json:"schema"`
	Operation  string                  `json:"operation"`
	ApprovalID string                  `json:"approval_id,omitempty"`
	Force      bool                    `json:"force,omitempty"`
	Settings   *autoupdate.Preferences `json:"settings,omitempty"`
}

// CandidateRejection names the downloaded version and why it failed verification.
type CandidateRejection struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

type ControlResponse struct {
	UpdaterVersion string                          `json:"updater_version,omitempty"`
	Schema         string                          `json:"schema"`
	Status         string                          `json:"status"`
	Version        string                          `json:"version,omitempty"`
	Updated        bool                            `json:"updated"`
	Pending        bool                            `json:"pending,omitempty"`
	Candidate      *workerupdate.PreparedCandidate `json:"candidate,omitempty"`
	// CandidateRejected reports a downloaded update that no longer verifies for a reason a user must know (an unsafe
	// file, another target, a signature that did not verify). A replaced or removed payload is not reported.
	CandidateRejected *CandidateRejection                `json:"candidate_rejected,omitempty"`
	ActivationFailure string                             `json:"activation_failure,omitempty"`
	Observation       autoupdate.Observation             `json:"observation"`
	ErrorCode         string                             `json:"error_code,omitempty"`
	ErrorMessage      string                             `json:"error_message,omitempty"`
	Transaction       workerupdate.TransactionState      `json:"transaction"`
	Settings          *autoupdate.Preferences            `json:"settings,omitempty"`
	NextMaintenanceAt time.Time                          `json:"next_maintenance_at,omitempty"`
	OwnerMaintenance  *autoupdate.OwnerMaintenanceNotice `json:"owner_maintenance,omitempty"`
}

// UpdateHealth is the availability value doctor reads: "recovery_required" when the updater errors or its journal
// shows a rollback that failed, an installed update whose commit never finished, or contradicts itself, otherwise "healthy". A pending activation and a rejected or
// quarantined candidate leave the running version fine, so they are not faults.
func (r ControlResponse) UpdateHealth() string {
	if r.Status != "ok" || r.Transaction.Failure == updateflow.FailureRollback || r.Transaction.Failure == updateflow.FailureContradictoryJournal || r.Transaction.Failure == updateflow.FailureCommit || r.Transaction.Failure == updateflow.FailureRevoked {
		return "recovery_required"
	}
	return "healthy"
}

func validControlRequest(request ControlRequest) bool {
	if request.Schema != "" && request.Schema != ControlProtocolV1 {
		return false
	}
	if request.Operation == "session-owner-start" {
		return !request.Force && request.ApprovalID == "" && request.Settings == nil && sessionowner.ValidGeneration(request.Generation)
	}
	if request.Generation != "" {
		return false
	}
	switch request.Operation {
	case "status", "check", "download":
		return !request.Force && request.ApprovalID == "" && request.Settings == nil
	case "settings":
		return !request.Force && request.ApprovalID == "" && (request.Settings == nil || request.Settings.Validate() == nil)
	case "install":
		return request.Settings == nil && approvalIDPattern.MatchString(request.ApprovalID)
	default:
		return false
	}
}

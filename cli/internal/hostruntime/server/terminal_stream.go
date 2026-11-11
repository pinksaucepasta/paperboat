package server

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// terminalResumeGrace is how long a native owner attachment outlives its closed connection, so the client can move it
// to a new connection with its resume secret. The host cannot tell a handover from a lost client; an attachment nobody
// resumes is detached when the grace ends.
var terminalResumeGrace = 10 * time.Second

type terminalOutputStream struct {
	attachmentLease    uint64
	resumable          bool
	manager            session.Service
	sessionID          string
	attachmentID       string
	expectedGeneration uint64
	checkpoint         []byte
	checkpointSequence uint64
	checkpointOffset   int
	checkpointSent     bool
	broadcast          *browserbroadcastserver.Registry
	keyUpdates         <-chan browserbroadcastserver.Epoch
	// screenRequests asks a browser stream for a fresh checkpoint of its own attachment (accountID, clientID,
	// generation); unregisterScreens withdraws it from the dispatcher when the stream closes.
	screenRequests chan struct{}
	// lagsSeen is the shared output's lag count when this stream last sent a screen for it.
	lagsSeen          uint64
	accountID         string
	clientID          string
	generation        uint64
	unregisterScreens func()
	// participants is the last participant list this stream reported; participantsCheckedAt
	// bounds how often a busy stream looks again.
	participants          string
	participantsCheckedAt time.Time
	// idleWait is how long the next wait for output lasts before the stream checks the session again.
	idleWait time.Duration
	// liveCheckedAt is when the stream last confirmed its session (liveChecked).
	liveCheckedAt time.Time
	// received is the end of the last output this stream returned; WaitNext frees output only up to it, so a reply
	// lost to a poll deadline is delivered again instead of skipped.
	received uint64
	// resumeSecret is handed once to the attach response, never kept by the stream.
	resumeSecret []byte
	// recordingID is the recording this stream last told its client about ("" for none), and
	// pendingRecording a change found by a liveness check, sent before the next output.
	recordingID      string
	pendingRecording error
}

// participantsInterval bounds how soon a viewer learns that someone joined, left or changed role.
const participantsInterval = 500 * time.Millisecond

// participantsEvent reports the session's participants when they changed since this stream last
// reported them, so viewers update their people list without polling the control plane. The first
// call reports the current list.
func (s *terminalOutputStream) participantsEvent() error {
	if time.Since(s.participantsCheckedAt) < participantsInterval {
		return nil
	}
	s.participantsCheckedAt = time.Now()
	snapshot, err := s.snapshot()
	if err != nil {
		return nil
	}
	if err := s.recordingEvent(snapshot); err != nil {
		return err
	}
	signature := participantSignature(snapshot.Participants)
	if signature == s.participants {
		return nil
	}
	s.participants = signature
	participants := snapshot.Participants
	if participants == nil {
		participants = []session.Participant{}
	}
	payload, err := json.Marshal(struct {
		Event        string                `json:"event"`
		SessionID    string                `json:"session_id"`
		Participants []session.Participant `json:"participants"`
	}{"participants", snapshot.ID, participants})
	if err != nil {
		return err
	}
	return &StreamEvent{Payload: payload}
}

func participantSignature(participants []session.Participant) string {
	entries := make([]string, 0, len(participants))
	for _, participant := range participants {
		entries = append(entries, participant.AttachmentID+"\x00"+participant.AccountID+"\x00"+participant.Role)
	}
	sort.Strings(entries)
	return "[" + strings.Join(entries, "\x01") + "]"
}

// An idle stream looks for the session's exit after terminalExitObservationInterval, then at doubling
// intervals up to terminalIdleObservationMaximum until output arrives again: polling every 100 ms with two
// snapshots made 1,000 idle viewers cost 20,000 snapshots a second. A shell almost always writes output
// just before it exits, which resets the wait, so an exit is still noticed within ~100 ms.
const (
	terminalExitObservationInterval = 100 * time.Millisecond
	terminalIdleObservationMaximum  = 2 * time.Second
)

func (d *Dispatcher) OpenStream(ctx context.Context, authorization Authorization, capability string, payload json.RawMessage, outcome operation.Outcome, replay bool) (OutputStream, bool, error) {
	if capability == "config.compare.v1" && outcome.ErrorCode == "" {
		return d.openConfigComparison(ctx, authorization, payload, outcome)
	}
	if capability == "ssh.v1" {
		return d.openSSHStream(ctx, authorization, payload, outcome)
	}
	if capability == "exec.v1" && outcome.ErrorCode == "" {
		var request execRequest
		if decodeStrict(payload, &request) != nil || request.OperationID == "" || request.Action != "start" && request.Action != "attach" {
			return nil, false, nil
		}
		execution, err := d.config.Exec.Get(request.OperationID)
		if err != nil {
			return nil, false, err
		}
		from := request.FromSequence
		if from == 0 {
			from = 1
		}
		reader, err := execution.OpenReader(from)
		if err != nil {
			return nil, false, err
		}
		return &execOutputStream{execution: execution, reader: reader}, true, nil
	}
	return d.openTerminalStream(ctx, authorization, capability, payload, outcome, replay)
}

func (d *Dispatcher) openTerminalStream(ctx context.Context, authorization Authorization, capability string, payload json.RawMessage, outcome operation.Outcome, replay bool) (OutputStream, bool, error) {
	if capability != "terminal.v1" || outcome.ErrorCode != "" {
		return nil, false, nil
	}
	var request terminalRequest
	if decodeStrict(payload, &request) != nil || request.Action != "attach" && request.Action != "replay" {
		return nil, false, nil
	}
	var response terminalAttachResponse
	if json.Unmarshal(outcome.Result, &response) != nil || response.AttachmentID == "" {
		return nil, false, ErrInvalidConfiguration
	}
	if authorization.BrowserTerminal && response.AttachmentID != authorization.BrowserAttachmentID {
		return nil, false, ErrInvalidConfiguration
	}
	if replay {
		attached, err := d.attachTerminalParticipant(authorization, request, response.AttachmentID)
		if err != nil {
			return nil, false, err
		}
		response.AttachmentLease = attached.AttachmentLease
	}

	expectedGeneration := uint64(0)
	if isGenerationBoundTerminal(authorization) {
		expectedGeneration = authorization.TerminalGeneration
	}
	generation := authorization.TerminalGeneration
	if generation == 0 {
		generation = response.Session.Snapshot.Generation
	}
	recordingID := ""
	if response.Session.Snapshot.Recording != nil {
		recordingID = response.Session.Snapshot.Recording.ID
	}
	stream := &terminalOutputStream{recordingID: recordingID, manager: d.config.Sessions, sessionID: request.SessionID, attachmentID: response.AttachmentID, attachmentLease: response.AttachmentLease, expectedGeneration: expectedGeneration, checkpointSent: true, accountID: authorization.AccountID, clientID: authorization.ClientID, generation: generation}
	if authorization.BrowserTerminal || request.AtLiveBoundary {
		if err := d.prepareTerminalCheckpoint(ctx, authorization, request, response, stream); err != nil {
			return nil, false, err
		}
	}
	if !isGenerationBoundTerminal(authorization) && authorization.ResourceID != "" {
		// A native owner attachment gets a fresh (rotated) resume secret each time it attaches. Without
		// one, a carrier move falls back to the operation credential.
		var issueErr error
		stream.resumeSecret, issueErr = d.config.Sessions.IssueResume(request.SessionID, response.AttachmentID, response.AttachmentLease, authorization.ResourceID)
		stream.resumable = issueErr == nil
	}
	return stream, true, nil
}

// resumeTerminal is the only request a resume connection may make before it proves the attachment's
// secret. Every refusal looks the same, so a caller learns nothing about which check failed.
func (d *Dispatcher) resumeTerminal(authorization Authorization, request terminalRequest) operation.Outcome {
	if request.Action != "attach" || request.SessionID != authorization.SessionID || request.AttachmentID == "" || len(request.ResumeSecret) != session.ResumeSecretSize {
		return failure("not_found_or_forbidden")
	}
	value, err := d.config.Sessions.ResumeAttachment(request.SessionID, session.ResumeRequest{AttachmentID: request.AttachmentID, AccountID: authorization.AccountID, ClientID: authorization.ClientID, Resource: authorization.Resume.Resource, Standing: authorization.Resume.Standing, Secret: request.ResumeSecret, FromSequence: request.FromSequence})
	if errors.Is(err, session.ErrInvalidInput) {
		return failure("not_found_or_forbidden")
	}
	if err != nil {
		errorreport.Current().ObserveFailure(context.Background(), "paperboat-daemon", "sessions", "resume_attachment", "terminal_resume_failed", err)
		return domainResult(nil, err)
	}
	authorization.Resume.resumed.Store(true)
	response := newTerminalAttachResponse(request.AttachmentID, value)
	if response.InputSequence, err = d.config.Sessions.InputSequence(request.SessionID, authorization.ClientID, request.AttachmentID, value.Snapshot.Generation); err != nil {
		errorreport.Current().ObserveFailure(context.Background(), "paperboat-daemon", "sessions", "input_sequence", "terminal_resume_failed", err)
		return domainResult(nil, err)
	}
	return result(response)
}

// Fresh native attachments and browsers use the same coherent VT checkpoint.
// Saved-cursor reconnects retain byte replay and never synthesize input.
func (d *Dispatcher) prepareTerminalCheckpoint(ctx context.Context, authorization Authorization, request terminalRequest, response terminalAttachResponse, stream *terminalOutputStream) error {
	generation := authorization.TerminalGeneration
	if generation == 0 {
		generation = response.Session.Snapshot.Generation
	}
	ready := false
	defer func() {
		if !ready {
			_ = d.config.Sessions.DetachAttachment(request.SessionID, response.AttachmentID, response.AttachmentLease)
		}
	}()
	checkpoint, boundary, err := d.config.Sessions.BrowserScreenAttachment(request.SessionID, response.AttachmentID, authorization.AccountID, authorization.ClientID, generation)
	if err != nil {
		return err
	}
	stream.checkpoint = checkpoint
	stream.checkpointSequence = boundary
	stream.checkpointSent = false
	if authorization.BrowserTerminal {
		if d.config.BrowserOutput == nil {
			return ErrCapabilityUnavailable
		}
		updates, err := d.config.BrowserOutput.Join(ctx, request.SessionID, generation, response.AttachmentID)
		if err != nil {
			return err
		}
		stream.broadcast = d.config.BrowserOutput
		stream.keyUpdates = updates
		requests := make(chan struct{}, 1)
		stream.screenRequests = requests
		d.browserScreens.Store(response.AttachmentID, requests)
		stream.unregisterScreens = func() { d.browserScreens.CompareAndDelete(response.AttachmentID, requests) }
	}
	ready = true
	return nil
}

type terminalAttachResponse struct {
	AttachmentLease uint64 `json:"attachment_lease"`
	StreamID        uint32 `json:"stream_id,omitempty"`
	AttachmentID    string `json:"attachment_id"`
	InputSequence   uint64 `json:"input_sequence,omitempty"`
	FirstAttachment bool   `json:"first_attachment,omitempty"`
	Session         struct {
		Snapshot session.Snapshot `json:"snapshot"`
		Replay   struct {
			FromSequence     uint64 `json:"from_sequence"`
			ToSequence       uint64 `json:"to_sequence"`
			EarliestSequence uint64 `json:"earliest_sequence"`
			LatestSequence   uint64 `json:"latest_sequence"`
		} `json:"replay"`
	} `json:"session"`
}

func newTerminalAttachResponse(attachmentID string, attached session.AttachResult) terminalAttachResponse {
	response := terminalAttachResponse{AttachmentLease: attached.AttachmentLease, AttachmentID: attachmentID, FirstAttachment: attached.FirstAttachment}
	response.Session.Snapshot = attached.Snapshot
	response.Session.Replay.FromSequence = attached.Replay.FromSequence
	response.Session.Replay.ToSequence = attached.Replay.ToSequence
	response.Session.Replay.EarliestSequence = attached.Replay.EarliestSequence
	response.Session.Replay.LatestSequence = attached.Replay.LatestSequence
	return response
}

// The normal request and journal replay bind the same authenticated participant.
func (d *Dispatcher) attachTerminalParticipant(authorization Authorization, request terminalRequest, attachmentID string) (session.AttachResult, error) {
	participant := session.Participant{SourceMachineID: authorization.SourceMachineID, AttachmentID: attachmentID, AccountID: authorization.AccountID, ClientID: authorization.ClientID, Role: string(authorization.TerminalRole), ConnectedAt: d.config.Now(), Browser: authorization.BrowserTerminal, Dimensions: pty.Dimensions{Columns: request.Columns, Rows: request.Rows}}
	if isGenerationBoundTerminal(authorization) {
		if request.AtLiveBoundary && authorization.BrowserTerminal {
			return d.config.Sessions.AttachLiveParticipantAtGeneration(request.SessionID, participant, authorization.TerminalGeneration)
		}
		return d.config.Sessions.AttachParticipantAtGeneration(request.SessionID, participant, request.FromSequence, authorization.TerminalGeneration)
	}
	participant.Role = string(TerminalRoleOwner)
	if request.Action == "attach" && request.AtLiveBoundary {
		return d.config.Sessions.AttachLiveParticipant(request.SessionID, participant)
	}
	return d.config.Sessions.AttachParticipant(request.SessionID, participant, request.FromSequence)
}

func (s *terminalOutputStream) Close() error {
	if s.unregisterScreens != nil {
		s.unregisterScreens()
	}
	if s.broadcast != nil {
		s.broadcast.Leave(s.sessionID, s.attachmentID)
	}
	if s.resumable {
		// A takeover by a resume changes the lease, which makes this later detach a no-op.
		manager, sessionID, attachmentID, lease := s.manager, s.sessionID, s.attachmentID, s.attachmentLease
		time.AfterFunc(terminalResumeGrace, func() { _ = manager.DetachAttachment(sessionID, attachmentID, lease) })
		return nil
	}
	return s.manager.DetachAttachment(s.sessionID, s.attachmentID, s.attachmentLease)
}

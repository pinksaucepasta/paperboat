package server

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type terminalOutputStream struct {
	manager            session.Service
	sessionID          string
	attachmentID       string
	expectedGeneration uint64
	writers            *filetransfer.WriterRegistry
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
		if _, err := d.attachTerminalParticipant(authorization, request, response.AttachmentID); err != nil {
			return nil, false, err
		}
	}

	expectedGeneration := uint64(0)
	if isGenerationBoundTerminal(authorization) {
		expectedGeneration = authorization.TerminalGeneration
	}
	generation := authorization.TerminalGeneration
	if generation == 0 {
		generation = response.Session.Snapshot.Generation
	}
	stream := &terminalOutputStream{manager: d.config.Sessions, sessionID: request.SessionID, attachmentID: response.AttachmentID, expectedGeneration: expectedGeneration, writers: d.config.Writers, checkpointSent: true, accountID: authorization.AccountID, clientID: authorization.ClientID, generation: generation}
	if authorization.BrowserTerminal || request.AtLiveBoundary {
		if err := d.prepareTerminalCheckpoint(ctx, authorization, request, response, stream); err != nil {
			return nil, false, err
		}
	}
	if d.config.Writers != nil {
		d.config.Writers.Attach(request.SessionID, response.AttachmentID, authorization.ClientID, authorization.SourceMachineID)
	}
	return stream, true, nil
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
			_ = d.config.Sessions.DetachParticipantAtGeneration(request.SessionID, response.AttachmentID, authorization.AccountID, authorization.ClientID, generation)
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
	response := terminalAttachResponse{AttachmentID: attachmentID, FirstAttachment: attached.FirstAttachment}
	response.Session.Snapshot = attached.Snapshot
	response.Session.Replay.FromSequence = attached.Replay.FromSequence
	response.Session.Replay.ToSequence = attached.Replay.ToSequence
	response.Session.Replay.EarliestSequence = attached.Replay.EarliestSequence
	response.Session.Replay.LatestSequence = attached.Replay.LatestSequence
	return response
}

// The normal request and journal replay bind the same authenticated participant.
func (d *Dispatcher) attachTerminalParticipant(authorization Authorization, request terminalRequest, attachmentID string) (session.AttachResult, error) {
	participant := session.Participant{AttachmentID: attachmentID, AccountID: authorization.AccountID, ClientID: authorization.ClientID, Role: string(authorization.TerminalRole), ConnectedAt: d.config.Now(), Browser: authorization.BrowserTerminal, Dimensions: pty.Dimensions{Columns: request.Columns, Rows: request.Rows}}
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

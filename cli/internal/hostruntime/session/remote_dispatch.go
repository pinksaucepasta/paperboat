package session

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"io"
	"time"
)

// ServeRemoteCall exposes only explicit session operations, never lifecycle shutdown.
func ServeRemoteCall(ctx context.Context, m Service, op string, body json.RawMessage) (json.RawMessage, error) {
	switch op {
	case "ResourceCounts":
		var args struct{}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0 := m.ResourceCounts()
		return json.Marshal(struct{ V0 map[string]uint64 }{v0})
	case "Create":
		var args struct {
			Request            CreateRequest
			ManagedEnvironment []string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.Create(WithLaunchEnvironment(ctx, args.ManagedEnvironment), args.Request)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "Attach":
		var args struct {
			Sessionid    string
			Attachmentid string
			Fromsequence uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.Attach(args.Sessionid, args.Attachmentid, args.Fromsequence)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 AttachResult }{v0})
	case "AttachParticipant":
		var args struct {
			Sessionid    string
			Participant  remoteParticipant
			Fromsequence uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.AttachParticipant(args.Sessionid, args.Participant.participant(), args.Fromsequence)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 AttachResult }{v0})
	case "AttachParticipantAtGeneration":
		var args struct {
			Sessionid          string
			Participant        remoteParticipant
			Fromsequence       uint64
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.AttachParticipantAtGeneration(args.Sessionid, args.Participant.participant(), args.Fromsequence, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 AttachResult }{v0})
	case "AttachLive":
		var args struct {
			Sessionid    string
			Attachmentid string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.AttachLive(args.Sessionid, args.Attachmentid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 AttachResult }{v0})
	case "AttachLiveParticipant":
		var args struct {
			Sessionid   string
			Participant remoteParticipant
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.AttachLiveParticipant(args.Sessionid, args.Participant.participant())
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 AttachResult }{v0})
	case "AttachLiveParticipantAtGeneration":
		var args struct {
			Sessionid          string
			Participant        remoteParticipant
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.AttachLiveParticipantAtGeneration(args.Sessionid, args.Participant.participant(), args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 AttachResult }{v0})
	case "BrowserScreenAttachment":
		var args struct {
			Sessionid          string
			Attachmentid       string
			Accountid          string
			Clientid           string
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, v1, err := m.BrowserScreenAttachment(args.Sessionid, args.Attachmentid, args.Accountid, args.Clientid, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			V0 []byte
			V1 uint64
		}{v0, v1})
	case "Detach":
		var args struct {
			Sessionid    string
			Attachmentid string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.Detach(args.Sessionid, args.Attachmentid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "DetachParticipantAtGeneration":
		var args struct {
			Sessionid          string
			Attachmentid       string
			Accountid          string
			Clientid           string
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.DetachParticipantAtGeneration(args.Sessionid, args.Attachmentid, args.Accountid, args.Clientid, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "AttachmentOwnedBy":
		var args struct {
			Sessionid    string
			Attachmentid string
			Accountid    string
			Clientid     string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0 := m.AttachmentOwnedBy(args.Sessionid, args.Attachmentid, args.Accountid, args.Clientid)
		return json.Marshal(struct{ V0 bool }{v0})
	case "Next":
		var args struct {
			Sessionid    string
			Attachmentid string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, v1, err := m.Next(args.Sessionid, args.Attachmentid)
		if err != nil {
			return nil, err
		}
		defer v0.Release()
		return json.Marshal(struct {
			V0 history.Event
			V1 bool
		}{v0, v1})
	case "WaitNext":
		var args struct {
			Sessionid    string
			Attachmentid string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.WaitNext(ctx, args.Sessionid, args.Attachmentid)
		if err != nil {
			return nil, err
		}
		defer v0.Release()
		return json.Marshal(struct{ V0 history.Event }{v0})
	case "Acknowledge":
		var args struct {
			Sessionid    string
			Attachmentid string
			Nextsequence uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.Acknowledge(args.Sessionid, args.Attachmentid, args.Nextsequence)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "AcknowledgeAtGeneration":
		var args struct {
			Sessionid          string
			Attachmentid       string
			Nextsequence       uint64
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.AcknowledgeAtGeneration(args.Sessionid, args.Attachmentid, args.Nextsequence, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "AttachmentStatus":
		var args struct {
			Sessionid    string
			Attachmentid string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, v1, err := m.AttachmentStatus(args.Sessionid, args.Attachmentid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			V0 AttachmentState
			V1 uint64
		}{v0, v1})
	case "Write":
		var args struct {
			Sessionid string
			Key       InputKey
			Data      []byte
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		owner, ok := m.(interface {
			WriteContext(context.Context, string, InputKey, []byte) (InputDecision, error)
		})
		if !ok {
			return nil, ErrInvalidSession
		}
		v0, err := owner.WriteContext(ctx, args.Sessionid, args.Key, args.Data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 InputDecision }{v0})
	case "InputSequence":
		var args struct {
			Sessionid    string
			Clientid     string
			Attachmentid string
			Generation   uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.InputSequence(args.Sessionid, args.Clientid, args.Attachmentid, args.Generation)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 uint64 }{v0})
	case "WriteStream":
		var args struct {
			Sessionid    string
			Attachmentid string
			Generation   uint64
			Data         []byte
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		owner, ok := m.(interface {
			WriteStreamContext(context.Context, string, string, uint64, []byte) error
		})
		if !ok {
			return nil, ErrInvalidSession
		}
		err := owner.WriteStreamContext(ctx, args.Sessionid, args.Attachmentid, args.Generation, args.Data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "QueryInput":
		var args struct {
			Sessionid string
			Key       InputKey
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.QueryInput(args.Sessionid, args.Key)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 InputDecision }{v0})
	case "Resize":
		var args struct {
			Sessionid    string
			Attachmentid string
			Dimensions   pty.Dimensions
			Activeat     time.Time
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.Resize(args.Sessionid, args.Attachmentid, args.Dimensions, args.Activeat)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "ResizeParticipantAtGeneration":
		var args struct {
			Sessionid          string
			Attachmentid       string
			Accountid          string
			Clientid           string
			Expectedgeneration uint64
			Dimensions         pty.Dimensions
			Activeat           time.Time
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.ResizeParticipantAtGeneration(args.Sessionid, args.Attachmentid, args.Accountid, args.Clientid, args.Expectedgeneration, args.Dimensions, args.Activeat)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "Signal":
		var args struct {
			Sessionid  string
			Generation uint64
			Signal     pty.Signal
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.Signal(args.Sessionid, args.Generation, args.Signal)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "Clear":
		var args struct{ Sessionid string }
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.Clear(args.Sessionid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 uint64 }{v0})
	case "ClearAtGeneration":
		var args struct {
			Sessionid          string
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.ClearAtGeneration(args.Sessionid, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 uint64 }{v0})
	case "Close":
		var args struct{ Sessionid string }
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.Close(ctx, args.Sessionid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "CloseAtGeneration":
		var args struct {
			Sessionid          string
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.CloseAtGeneration(ctx, args.Sessionid, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "Restart":
		var args struct{ Sessionid string }
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.Restart(args.Sessionid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "RestartAtGeneration":
		var args struct {
			Sessionid          string
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.RestartAtGeneration(args.Sessionid, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "RestartContext":
		var args struct {
			Sessionid          string
			ManagedEnvironment []string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.RestartContext(WithLaunchEnvironment(ctx, args.ManagedEnvironment), args.Sessionid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "RestartAtGenerationContext":
		var args struct {
			Sessionid          string
			Expectedgeneration uint64
			ManagedEnvironment []string
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.RestartAtGenerationContext(WithLaunchEnvironment(ctx, args.ManagedEnvironment), args.Sessionid, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "Delete":
		var args struct{ Sessionid string }
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.Delete(args.Sessionid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "DeleteAtGeneration":
		var args struct {
			Sessionid          string
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		err := m.DeleteAtGeneration(args.Sessionid, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{}{})
	case "Snapshot":
		var args struct{ Sessionid string }
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.Snapshot(args.Sessionid)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "SnapshotAtGeneration":
		var args struct {
			Sessionid          string
			Expectedgeneration uint64
		}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0, err := m.SnapshotAtGeneration(args.Sessionid, args.Expectedgeneration)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 Snapshot }{v0})
	case "List":
		var args struct{}
		if err := decodeRemoteRequest(body, &args); err != nil {
			return nil, ErrInvalidSession
		}
		v0 := m.List()
		return json.Marshal(struct{ V0 []Snapshot }{v0})
	default:
		return nil, ErrInvalidSession
	}
}

func decodeRemoteRequest(body []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrInvalidSession
	}
	return nil
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

type terminalSecurityHandler struct {
	inputs     int
	acks       int
	resizes    int
	activities int
}

func (h *terminalSecurityHandler) Handle(context.Context, Authorization, string, json.RawMessage) operation.Outcome {
	return operation.Outcome{}
}
func (h *terminalSecurityHandler) HandleTerminalInput(context.Context, Authorization, string, string, uint64, []byte) error {
	h.inputs++
	return nil
}
func (h *terminalSecurityHandler) HandleTerminalACK(context.Context, Authorization, string, string, uint64) error {
	h.acks++
	return nil
}
func (h *terminalSecurityHandler) HandleTerminalResize(context.Context, Authorization, string, string, uint16, uint16) error {
	h.resizes++
	return nil
}

func (h *terminalSecurityHandler) HandleTerminalActivity(context.Context, Authorization, string, string) error {
	h.activities++
	return nil
}

func boundTerminalState(streamID uint32) *terminalConnectionState {
	state := newTerminalConnectionState()
	state.streams[streamID] = &terminalStreamBinding{authorization: Authorization{ClientID: "cli_1"}, sessionID: "ses_1", attachmentID: "att_1", generation: 1}
	return state
}

func requireInvalidTerminalFrame(t *testing.T, err error) {
	t.Helper()
	var protocolErr *protocol.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.InvalidFrame {
		t.Fatalf("error=%v, want invalid terminal frame", err)
	}
}

func TestTerminalStreamIDsCannotCrossConnections(t *testing.T) {
	handler := &terminalSecurityHandler{}
	server := &Server{config: Config{Handler: handler}}
	first := boundTerminalState(7)
	second := boundTerminalState(9)
	message, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: 7, Sequence: 1, Data: []byte("x")}, nil)
	if err := server.handleTerminalData(context.Background(), message, first); err != nil {
		t.Fatal(err)
	}
	requireInvalidTerminalFrame(t, server.handleTerminalData(context.Background(), message, second))
	if handler.inputs != 1 {
		t.Fatalf("input calls=%d", handler.inputs)
	}
}

func TestLateFramesForClosedTerminalStreamDoNotKillCarrier(t *testing.T) {
	handler := &terminalSecurityHandler{}
	server := &Server{config: Config{Handler: handler}}
	state := boundTerminalState(7)
	state.remove(7)

	input, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: 7, Sequence: 1, Data: []byte("late")}, nil)
	ack, _ := protocol.EncodeTerminalACK(protocol.TerminalACKFrame{StreamID: 7, NextSequence: 1}, nil)
	resize, _ := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: 7, Columns: 80, Rows: 24, Sequence: 1}, nil)
	for _, frame := range [][]byte{input, ack, resize} {
		if err := server.handleTerminalData(context.Background(), frame, state); err != nil {
			t.Fatalf("late frame error=%v", err)
		}
	}
	unknown, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: 8, Sequence: 1, Data: []byte("unknown")}, nil)
	requireInvalidTerminalFrame(t, server.handleTerminalData(context.Background(), unknown, state))
	if handler.inputs != 0 || handler.acks != 0 || handler.resizes != 0 {
		t.Fatalf("handler calls: input=%d ack=%d resize=%d", handler.inputs, handler.acks, handler.resizes)
	}
}

func TestTerminalInputAndResizeSequencesMustBeContiguous(t *testing.T) {
	handler := &terminalSecurityHandler{}
	server := &Server{config: Config{Handler: handler}}
	state := boundTerminalState(7)

	input, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: 7, Sequence: 1, Data: []byte("x")}, nil)
	if err := server.handleTerminalData(context.Background(), input, state); err != nil {
		t.Fatal(err)
	}
	requireInvalidTerminalFrame(t, server.handleTerminalData(context.Background(), input, state))
	gap, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: 7, Sequence: 3, Data: []byte("x")}, nil)
	requireInvalidTerminalFrame(t, server.handleTerminalData(context.Background(), gap, state))

	resize, _ := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: 7, Columns: 120, Rows: 40, Sequence: 1}, nil)
	if err := server.handleTerminalData(context.Background(), resize, state); err != nil {
		t.Fatal(err)
	}
	requireInvalidTerminalFrame(t, server.handleTerminalData(context.Background(), resize, state))
	resizeGap, _ := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: 7, Columns: 80, Rows: 24, Sequence: 3}, nil)
	requireInvalidTerminalFrame(t, server.handleTerminalData(context.Background(), resizeGap, state))

	if handler.inputs != 1 || handler.resizes != 1 {
		t.Fatalf("input calls=%d resize calls=%d", handler.inputs, handler.resizes)
	}
}

// A focus frame reaches the handler as the window's activity; it has no sequence of its own and a bad frame is refused.
func TestTerminalActivityFrameReachesTheHandler(t *testing.T) {
	handler := &terminalSecurityHandler{}
	server := &Server{config: Config{Handler: handler}}
	state := boundTerminalState(7)
	frame, _ := protocol.EncodeTerminalActivity(protocol.TerminalActivityFrame{StreamID: 7}, nil)
	for range 2 {
		if err := server.handleTerminalData(context.Background(), frame, state); err != nil {
			t.Fatal(err)
		}
	}
	if handler.activities != 2 {
		t.Fatalf("activity calls=%d, want 2", handler.activities)
	}
	requireInvalidTerminalFrame(t, server.handleTerminalData(context.Background(), append(frame, 0), state))
}

func TestRevokedTerminalBindingRejectsDataFrames(t *testing.T) {
	handler := &terminalSecurityHandler{}
	server := &Server{config: Config{Handler: handler}}
	state := boundTerminalState(7)
	flag := new(atomic.Bool)
	state.streams[7].revoked = flag
	flag.Store(true)

	input, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: 7, Sequence: 1, Data: []byte("x")}, nil)
	ack, _ := protocol.EncodeTerminalACK(protocol.TerminalACKFrame{StreamID: 7, NextSequence: 1}, nil)
	resize, _ := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: 7, Columns: 80, Rows: 24, Sequence: 1}, nil)
	for _, frame := range [][]byte{input, ack, resize} {
		var protocolErr *protocol.Error
		err := server.handleTerminalData(context.Background(), frame, state)
		if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.CredentialExpired {
			t.Fatalf("error=%v, want credential expired", err)
		}
	}
	if handler.inputs != 0 || handler.acks != 0 || handler.resizes != 0 {
		t.Fatalf("handler calls: input=%d ack=%d resize=%d", handler.inputs, handler.acks, handler.resizes)
	}
}

type interruptRecorder struct {
	terminalSecurityHandler
	keys []uint64
}

func (h *interruptRecorder) InterruptTerminalInput(_ context.Context, _ Authorization, _, _ string, _, sequence uint64, _ byte) bool {
	h.keys = append(h.keys, sequence)
	return true
}

// While the frame worker is stuck writing input, the read loop offers a lone byte of the next input frame as an
// interrupt; longer input, already written sequences and other frames are left to the worker.
func TestBusyStreamOffersALoneByteAsAnInterrupt(t *testing.T) {
	handler := &interruptRecorder{}
	server := &Server{config: Config{Handler: handler}}
	state := boundTerminalState(7)
	state.streams[7].inputSequence = 4
	for _, frame := range []protocol.TerminalInputFrame{
		{StreamID: 7, Sequence: 5, Data: []byte{3}},
		{StreamID: 7, Sequence: 5, Data: []byte("ls")},
		{StreamID: 7, Sequence: 5, Data: []byte("x")}, // a printable key is never an interrupt (N11)
		{StreamID: 7, Sequence: 4, Data: []byte{3}},
		{StreamID: 9, Sequence: 5, Data: []byte{3}},
	} {
		message, _ := protocol.EncodeTerminalInput(frame, nil)
		server.interruptTerminalInput(context.Background(), message, state)
	}
	resize, _ := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: 7, Columns: 80, Rows: 24, Sequence: 1}, nil)
	server.interruptTerminalInput(context.Background(), resize, state)
	if len(handler.keys) != 1 || handler.keys[0] != 5 {
		t.Fatalf("interrupts offered: %v", handler.keys)
	}
}

// A lone Ctrl-C reaches the interrupt handler while the worker is idle only on a host that needs it (Windows, where a pipe ETX does
// not interrupt the running program); on other hosts the tty's own line discipline handles it. Before the change an idle worker never offered it.
func TestIdleWorkerOffersALoneCtrlCOnlyWhereTheHostNeedsIt(t *testing.T) {
	ctrlC, _ := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: 7, Sequence: 5, Data: []byte{3}}, nil)
	defer func(previous bool) { interruptWhenIdle = previous }(interruptWhenIdle)
	for _, test := range []struct {
		idleHost, busy bool
		want           int
	}{{false, true, 1}, {false, false, 0}, {true, false, 1}, {true, true, 1}} {
		interruptWhenIdle = test.idleHost
		handler := &interruptRecorder{}
		server := &Server{config: Config{Handler: handler}}
		state := boundTerminalState(7)
		state.streams[7].inputSequence = 4
		server.offerInterrupt(context.Background(), ctrlC, state, test.busy)
		if len(handler.keys) != test.want {
			t.Errorf("idle-host=%v busy=%v: interrupts offered %d, want %d", test.idleHost, test.busy, len(handler.keys), test.want)
		}
	}
}

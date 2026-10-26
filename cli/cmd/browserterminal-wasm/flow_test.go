package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"testing"
	"time"
)

func TestTerminalWriterAdmissionIsBoundedAndOrdered(t *testing.T) {
	q := newTerminalWriterQueue()
	first := terminalWrite{bytes: terminalWriteQueueBytes, payloads: [][]byte{[]byte("first")}}
	if err := q.enqueue(first); err != nil {
		t.Fatal(err)
	}
	active := <-q.items
	if err := q.enqueue(terminalWrite{bytes: 1}); err == nil {
		t.Fatal("active socket write released its byte reservation")
	}
	q.release(active)
	for i := 0; i < terminalWriteQueueDepth; i++ {
		if err := q.enqueue(terminalWrite{bytes: 1, kind: byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.enqueue(terminalWrite{bytes: 1}); err == nil {
		t.Fatal("queue exceeded item bound")
	}
	for i := 0; i < terminalWriteQueueDepth; i++ {
		item := <-q.items
		if item.kind != byte(i) {
			t.Fatal("writes reordered")
		}
		q.release(item)
	}
	q.close()
	if err := q.enqueue(first); err == nil {
		t.Fatal("closed queue accepted input")
	}
}

func TestTerminalACKCoalescesLatestSequence(t *testing.T) {
	q := newTerminalWriterQueue()
	for _, sequence := range []uint64{10, 20, 15} {
		if err := q.ack(sequence); err != nil {
			t.Fatal(err)
		}
	}
	if got := q.pendingACK(); got != 20 {
		t.Fatalf("pending = %d", got)
	}
	if len(q.ackNotify) != 1 {
		t.Fatal("ACK notifications did not coalesce")
	}
	<-q.ackNotify
	if err := q.ack(64 << 10); err != nil {
		t.Fatal(err)
	}
	select {
	case <-q.ackNotify:
	default:
		t.Fatal("ACK did not wake the writer")
	}
	q.sentACK(64 << 10)
	if got := q.pendingACK(); got != 0 {
		t.Fatalf("already acknowledged sequence pending = %d", got)
	}
	if err := q.ack(1); err != nil {
		t.Fatal(err)
	}
	if got := q.pendingACK(); got != 0 {
		t.Fatal("ACK regressed")
	}
	q.close()
	if err := q.ack(64<<10 + 1); err == nil {
		t.Fatal("closed queue accepted ACK")
	}
}

func TestTerminalWriterCloseDropsQueuedPayloadsAndPreservesActiveReservation(t *testing.T) {
	q := newTerminalWriterQueue()
	if err := q.enqueue(terminalWrite{bytes: 7}); err != nil {
		t.Fatal(err)
	}
	active := <-q.items
	if err := q.enqueue(terminalWrite{bytes: 11}); err != nil {
		t.Fatal(err)
	}
	q.close()
	if len(q.items) != 0 || q.bytes != 7 {
		t.Fatalf("close retained queue: items=%d bytes=%d", len(q.items), q.bytes)
	}
	q.release(active)
	if q.bytes != 0 {
		t.Fatal("active reservation leaked")
	}
}

func TestTerminalReceiveBudgetCoversPendingRecordsBeforeCopy(t *testing.T) {
	q := newTerminalReceiveQueue()
	copyOK := func(data []byte) bool { return true }
	if err := q.copyAndEnqueue(terminalBroadcastQueueBytes-terminalBroadcastRecordMetadataBytes, copyOK); err != nil {
		t.Fatal(err)
	}
	pending := <-q.items
	copied := false
	if err := q.copyAndEnqueue(1, func([]byte) bool { copied = true; return true }); err == nil || copied {
		t.Fatal("pending ciphertext escaped budget or rejected record was copied")
	}
	q.release(pending)
	if err := q.copyAndEnqueue(7, copyOK); err != nil {
		t.Fatal(err)
	}
	q.close()
	if q.bytes != 0 || len(q.items) != 0 {
		t.Fatal("queued ciphertext leaked on close")
	}
	if err := q.copyAndEnqueue(1, copyOK); err == nil {
		t.Fatal("closed queue admitted ciphertext")
	}
}

func TestTerminalReceiveCopyFailureAndQueueCapacityRelease(t *testing.T) {
	q := newTerminalReceiveQueue()
	if err := q.copyAndEnqueue(13, func([]byte) bool { return false }); err == nil || q.bytes != 0 || len(q.items) != 0 {
		t.Fatal("partial copy reserved ciphertext")
	}
	for i := 0; i < terminalBroadcastQueueBytes/(1+terminalBroadcastRecordMetadataBytes); i++ {
		if err := q.copyAndEnqueue(1, func([]byte) bool { return true }); err != nil {
			t.Fatal(err)
		}
	}
	copied := false
	if err := q.copyAndEnqueue(1, func([]byte) bool { copied = true; return true }); err == nil || copied {
		t.Fatal("full queue copied or admitted ciphertext")
	}
	active := <-q.items
	q.close()
	if q.bytes != len(active)+terminalBroadcastRecordMetadataBytes {
		t.Fatal("close dropped active record reservation")
	}
	q.release(active)
	if q.bytes != 0 {
		t.Fatal("active ciphertext leaked after close")
	}
}

func TestTerminalReceiveAcceptsMoreThan256ValidTinyRecords(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := browserbroadcast.NewEpoch()
	if err != nil {
		t.Fatal(err)
	}
	q := newTerminalReceiveQueue()
	defer q.close()
	for i := 0; i < 512; i++ {
		raw, err := browserbroadcast.Seal(browserbroadcast.Record{SessionID: "umts_burst", Generation: 1, EpochID: epoch.ID, Index: uint64(i + 1), Channel: protocol.TerminalStdout, StartSequence: uint64(i), Data: []byte("x")}, epoch, private)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.copyAndEnqueue(len(raw), func(dst []byte) bool { return copy(dst, raw) == len(raw) }); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	for i := 0; i < 512; i++ {
		raw := <-q.items
		record, err := browserbroadcast.Open(raw, epoch, public)
		if err != nil || record.Index != uint64(i+1) {
			t.Fatalf("record %d reordered or invalid: %v", i, err)
		}
		q.release(raw)
	}
	if q.bytes != 0 {
		t.Fatal("tiny-record reservations leaked")
	}
}

type recordingDeadlineConn struct{ deadlines []time.Time }

func (c *recordingDeadlineConn) SetDeadline(deadline time.Time) error {
	c.deadlines = append(c.deadlines, deadline)
	return nil
}

// F3-07: one fresh browser terminal stayed on "Connecting" for over a minute: after the TLS
// handshake nothing bounded hello, welcome, attach and checkpoint. The setup runs under one
// deadline that is cleared afterwards, also when setup fails.
func TestAttachSetupRunsUnderOneDeadlineThenClearsIt(t *testing.T) {
	conn := &recordingDeadlineConn{}
	started := time.Now()
	failure := errors.New("attach stalled")
	if err := withAttachDeadline(conn, attachSetupTimeout, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("setup error = %v", err)
	}
	if len(conn.deadlines) != 2 || conn.deadlines[0].Before(started.Add(attachSetupTimeout)) || !conn.deadlines[1].IsZero() {
		t.Fatalf("deadlines = %v; want setup deadline then cleared", conn.deadlines)
	}
	if attachSetupTimeout <= 0 || attachSetupTimeout > 30*time.Second {
		t.Fatalf("attach setup timeout %v must be bounded", attachSetupTimeout)
	}
}

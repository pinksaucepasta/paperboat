//go:build darwin || linux

package browserbroadcastserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

func TestOnePublisherServesOneHundredParticipantsAndRotatesOnDeparture(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 1, MaxAttachments: 128, HistoryBytes: 1 << 20, AttachmentBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	}()
	shell := "/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		t.Skip("requires /bin/sh")
	}
	created, err := manager.Create(context.Background(), session.CreateRequest{Name: "shared", Command: pty.Command{Path: shell, Args: []string{"-c", "stty -echo; cat"}, CWD: root, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(manager, func(context.Context) (ed25519.PrivateKey, error) { return private, nil })
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	var opens atomic.Int32
	r.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { opens.Add(1); return writer, nil })
	var first, second <-chan Epoch
	var epoch Epoch
	for i := 0; i < 100; i++ {
		id := "browser_" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		role := "interactive"
		if i == 0 {
			role = "owner"
		}
		if _, err := manager.AttachLiveParticipantAtGeneration(created.ID, session.Participant{AttachmentID: id, AccountID: fmt.Sprintf("account_%03d", i), ClientID: id, Role: role, ConnectedAt: time.Now(), Browser: true}, created.Generation); err != nil {
			t.Fatalf("participant %d: %v", i, err)
		}
		updates, err := r.Join(context.Background(), created.ID, created.Generation, id)
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		current := <-updates
		if i == 0 {
			first = updates
			epoch = current
		}
		if i == 1 {
			second = updates
		}
		if current.ID != epoch.ID {
			t.Fatal("participants received different output epochs")
		}
	}
	if opens.Load() != 1 {
		t.Fatalf("publisher opens=%d", opens.Load())
	}
	if _, err := r.Join(context.Background(), created.ID, created.Generation, "browser_overflow"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("101st participant accepted: %v", err)
	}
	_ = first
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan []byte, 1)
	go func() {
		var output []byte
		for {
			var header [4]byte
			if _, err := io.ReadFull(reader, header[:]); err != nil {
				result <- nil
				return
			}
			n := binary.BigEndian.Uint32(header[:])
			if n == 0 || n > browserbroadcast.MaxRecordBytes {
				result <- nil
				return
			}
			raw := make([]byte, n)
			if _, err := io.ReadFull(reader, raw); err != nil {
				result <- nil
				return
			}
			record, err := browserbroadcast.Open(raw, epoch, public)
			if err != nil {
				result <- nil
				return
			}
			output = append(output, record.Data...)
			complete := true
			for i := 0; i < 100; i++ {
				if !strings.Contains(string(output), fmt.Sprintf("<writer-%03d>", i)) {
					complete = false
					break
				}
			}
			if complete {
				result <- output
				return
			}
		}
	}()
	var writers sync.WaitGroup
	writeErrors := make(chan error, 100)
	for i := 0; i < 100; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			id := "browser_" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			_, err := manager.Write(created.ID, session.InputKey{ClientID: id, AttachmentID: id, Generation: created.Generation, InputSequence: 1}, []byte(fmt.Sprintf("<writer-%03d>\n", i)))
			if err != nil {
				writeErrors <- err
			}
		}(i)
	}
	writers.Wait()
	close(writeErrors)
	for err := range writeErrors {
		t.Fatal(err)
	}
	select {
	case data := <-result:
		if data == nil {
			t.Fatal("shared output was not machine-authenticated")
		}
	case <-ctx.Done():
		t.Fatal("shared output did not arrive")
	}
	r.Leave(created.ID, "browser_aa")
	rotated := <-second
	if rotated.ID == epoch.ID {
		t.Fatal("viewer departure did not rotate output key")
	}
	if err := manager.Detach(created.ID, "browser_aa"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Write(created.ID, session.InputKey{ClientID: "browser_ab", AttachmentID: "browser_ab", Generation: created.Generation, InputSequence: 2}, []byte("<after-revoke>\n")); err != nil {
		t.Fatal(err)
	}
	// Output produced before the departure may still be in the stream under the old key (the pty can
	// hand the tail of a line over as its own record). What matters: nothing produced after the
	// departure opens with the departed key, and the remaining participants read it with the new one.
	afterRevoke := make(chan error, 1)
	go func() {
		rotatedSeen := false
		for {
			var header [4]byte
			if _, err := io.ReadFull(reader, header[:]); err != nil {
				afterRevoke <- fmt.Errorf("missing output after revocation: %w", err)
				return
			}
			n := binary.BigEndian.Uint32(header[:])
			if n == 0 || n > browserbroadcast.MaxRecordBytes {
				afterRevoke <- errors.New("invalid record length")
				return
			}
			raw := make([]byte, n)
			if _, err := io.ReadFull(reader, raw); err != nil {
				afterRevoke <- err
				return
			}
			if old, err := browserbroadcast.Open(raw, epoch, public); err == nil {
				if rotatedSeen || strings.Contains(string(old.Data), "<after-revoke>") {
					afterRevoke <- errors.New("departed participant's key decrypted future output")
					return
				}
				continue
			}
			record, err := browserbroadcast.Open(raw, rotated, public)
			if err != nil {
				afterRevoke <- fmt.Errorf("remaining participant could not decrypt future output: %w", err)
				return
			}
			rotatedSeen = true
			if strings.Contains(string(record.Data), "<after-revoke>") {
				afterRevoke <- nil
				return
			}
		}
	}()
	select {
	case err := <-afterRevoke:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("output after revocation did not arrive")
	}
	r.SetPublisher(nil)
}

type discardPublisher struct{}

func (discardPublisher) Write(data []byte) (int, error) { return len(data), nil }
func (discardPublisher) Close() error                   { return nil }

func TestPublisherFailoverClosesOldEpochAndStartsFreshOutput(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 1, MaxAttachments: 4, HistoryBytes: 1 << 20, AttachmentBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	}()
	created, err := manager.Create(context.Background(), session.CreateRequest{Name: "failover", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "stty -echo; cat"}, CWD: root, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(manager, func(context.Context) (ed25519.PrivateKey, error) { return private, nil })
	if err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	open := func(context.Context, string) (io.WriteCloser, error) { opens.Add(1); return discardPublisher{}, nil }
	registry.SetPublisher(open)
	oldUpdates, err := registry.Join(context.Background(), created.ID, created.Generation, "viewer_old")
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := <-oldUpdates
	registry.SetPublisher(open)
	if _, ok := <-oldUpdates; ok {
		t.Fatal("old edge retained an output key stream")
	}
	newUpdates, err := registry.Join(context.Background(), created.ID, created.Generation, "viewer_new")
	if err != nil {
		t.Fatal(err)
	}
	newEpoch := <-newUpdates
	if newEpoch.ID == oldEpoch.ID || opens.Load() != 2 {
		t.Fatalf("failover epoch reused or publisher count=%d", opens.Load())
	}
	registry.SetPublisher(nil)
}

type panicUnwrapPublisherError struct{}

func (*panicUnwrapPublisherError) Error() string {
	return "private publisher failure: browser-private-marker"
}
func (*panicUnwrapPublisherError) Unwrap() error { panic("publisher error unwrap failed") }

type uncomparablePublisherError []byte

func (err uncomparablePublisherError) Error() string { return string(err) }

func TestPublisherDeliveryFailureIsCapturedOnceWithSafeCauseAndReference(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{
		Launch:          func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) },
		MaxSessions:     1,
		MaxAttachments:  8,
		HistoryBytes:    1 << 20,
		AttachmentBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer func() { _ = manager.Shutdown(ctx) }()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	created, err := manager.Create(ctx, session.CreateRequest{
		Name: "publisher-failure",
		Command: pty.Command{
			Path:       "/bin/sh",
			Args:       []string{"-c", "stty -echo; cat"},
			CWD:        root,
			Env:        []string{"PATH=/usr/bin:/bin", "TERM=xterm"},
			Dimensions: pty.Dimensions{Columns: 80, Rows: 24},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	originalPrivate := append(ed25519.PrivateKey(nil), private...)
	registry, err := NewRegistry(manager, func(context.Context) (ed25519.PrivateKey, error) { return private, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer registry.SetPublisher(nil)
	secretCause := &panicUnwrapPublisherError{}
	failed := &causePublisher{failure: secretCause, closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) {
		return failed, nil
	})

	observed := make(chan errorreport.Fault, 8)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { observed <- fault })
	defer restore()
	reference := supportref.New()
	joinCtx := supportref.WithContext(context.Background(), reference)
	attachTestViewer(t, manager, created.ID, created.Generation, "browser_viewer")
	joinUpdates, err := registry.Join(joinCtx, created.ID, created.Generation, "browser_viewer")
	if err != nil {
		t.Fatal(err)
	}
	failedEpoch := <-joinUpdates
	writeTestViewer(t, manager, created.ID, created.Generation, "browser_viewer", 1)
	select {
	case fault := <-observed:
		if fault.Operation != "sessions" || fault.Stage != deliveryStage || fault.Code != sessionCode || fault.SupportReference != reference {
			t.Fatalf("publisher fault classification = %#v", fault)
		}
		serialized := strings.Join([]string{fault.Component, fault.Operation, fault.Stage, fault.Code, fault.Cause, fault.ErrorType, fault.SupportReference, strings.Join(fault.ErrorChain, ",")}, "|")
		if strings.Contains(serialized, "browser-private-marker") || strings.Contains(serialized, "private publisher failure") {
			t.Fatalf("fault retained private text: %q", serialized)
		}
	case <-ctx.Done():
		t.Fatal("publisher failure was not reported")
	}
	waitPublisherClosed(t, failed.closed)
	if !bytes.Equal(private, originalPrivate) {
		t.Fatal("publisher cleanup modified the shared signing identity")
	}
	select {
	case fault := <-observed:
		t.Fatalf("publisher failure was captured more than once: %#v", fault)
	case <-time.After(100 * time.Millisecond):
	}

	healthy := &signalPublisher{writes: make(chan struct{}, 4), closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return healthy, nil })
	attachTestViewer(t, manager, created.ID, created.Generation, "browser_recovered")
	recovered, err := registry.Join(joinCtx, created.ID, created.Generation, "browser_recovered")
	if err != nil {
		t.Fatalf("same registry did not accept a fresh viewer after publisher failure: %v", err)
	}
	recoveredEpoch := <-recovered
	if recoveredEpoch.ID == failedEpoch.ID {
		t.Fatal("recovered publisher reused the failed epoch")
	}
	writeTestViewer(t, manager, created.ID, created.Generation, "browser_recovered", 1)
	select {
	case <-healthy.writes:
	case <-time.After(2 * time.Second):
		t.Fatal("fresh publisher did not deliver output after recovery")
	}
	select {
	case fault := <-observed:
		t.Fatalf("recovery produced another fault: %#v", fault)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublisherWriteContractFailuresAreCapturedAndRecover(t *testing.T) {
	cases := []struct {
		name   string
		open   func() io.WriteCloser
		closed func(io.WriteCloser) <-chan struct{}
	}{
		{
			name: "short header write",
			open: func() io.WriteCloser { return &shortHeaderPublisher{closed: make(chan struct{})} },
			closed: func(writer io.WriteCloser) <-chan struct{} {
				return writer.(*shortHeaderPublisher).closed
			},
		},
		{
			name: "write deadline reset",
			open: func() io.WriteCloser { return &deadlineResetPublisher{closed: make(chan struct{})} },
			closed: func(writer io.WriteCloser) <-chan struct{} {
				return writer.(*deadlineResetPublisher).closed
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			manager, sessionID, generation, registry := newPublisherTestSession(t)
			observed := make(chan errorreport.Fault, 8)
			restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { observed <- fault })
			defer restore()
			reference := supportref.New()
			joinCtx := supportref.WithContext(context.Background(), reference)
			failed := test.open()
			registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return failed, nil })
			attachTestViewer(t, manager, sessionID, generation, "browser_failed")
			updates, err := registry.Join(joinCtx, sessionID, generation, "browser_failed")
			if err != nil {
				t.Fatal(err)
			}
			failedEpoch := <-updates
			writeTestViewer(t, manager, sessionID, generation, "browser_failed", 1)
			fault := waitPublisherFault(t, observed)
			if fault.Operation != "sessions" || fault.Stage != deliveryStage || fault.Code != sessionCode || fault.SupportReference != reference {
				t.Fatalf("publisher fault classification = %#v", fault)
			}
			if fault.Cause == "" || fault.Cause == "canceled" {
				t.Fatalf("publisher write failure lost its operational cause: %#v", fault)
			}
			serialized := strings.Join([]string{fault.Component, fault.Operation, fault.Stage, fault.Code, fault.Cause, fault.ErrorType, fault.SupportReference, strings.Join(fault.ErrorChain, ",")}, "|")
			if strings.Contains(serialized, "private deadline reset marker") {
				t.Fatalf("fault retained private writer text: %q", serialized)
			}
			waitPublisherClosed(t, test.closed(failed))

			healthy := &signalPublisher{writes: make(chan struct{}, 4), closed: make(chan struct{})}
			registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return healthy, nil })
			attachTestViewer(t, manager, sessionID, generation, "browser_recovered")
			recovered, err := registry.Join(joinCtx, sessionID, generation, "browser_recovered")
			if err != nil {
				t.Fatalf("same registry did not recover: %v", err)
			}
			recoveredEpoch := <-recovered
			if recoveredEpoch.ID == failedEpoch.ID {
				t.Fatal("recovered publisher reused the failed epoch")
			}
			writeTestViewer(t, manager, sessionID, generation, "browser_recovered", 1)
			select {
			case <-healthy.writes:
			case <-time.After(2 * time.Second):
				t.Fatal("fresh publisher did not deliver output after recovery")
			}
			select {
			case fault := <-observed:
				t.Fatalf("recovery produced another fault: %#v", fault)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestPublisherPureCancellationIsQuietButMixedFailureIsObserved(t *testing.T) {
	manager, sessionID, generation, registry := newPublisherTestSession(t)
	observed := make(chan errorreport.Fault, 8)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { observed <- fault })
	defer restore()
	reference := supportref.New()
	joinCtx := supportref.WithContext(context.Background(), reference)

	ordinary := &causePublisher{failure: context.Canceled, closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return ordinary, nil })
	attachTestViewer(t, manager, sessionID, generation, "browser_cancelled")
	if _, err := registry.Join(joinCtx, sessionID, generation, "browser_cancelled"); err != nil {
		t.Fatal(err)
	}
	writeTestViewer(t, manager, sessionID, generation, "browser_cancelled", 1)
	waitPublisherClosed(t, ordinary.closed)
	if len(observed) != 0 {
		t.Fatalf("ordinary publisher cancellation was reported: %#v", <-observed)
	}

	mixed := &causePublisher{failure: errors.Join(context.Canceled, uncomparablePublisherError("private uncomparable marker"), syscall.EIO), closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return mixed, nil })
	attachTestViewer(t, manager, sessionID, generation, "browser_mixed")
	if _, err := registry.Join(joinCtx, sessionID, generation, "browser_mixed"); err != nil {
		t.Fatal(err)
	}
	writeTestViewer(t, manager, sessionID, generation, "browser_mixed", 1)
	fault := waitPublisherFault(t, observed)
	if fault.Operation != "sessions" || fault.Stage != deliveryStage || fault.Code != sessionCode || fault.SupportReference != reference || fault.Cause == "canceled" {
		t.Fatalf("mixed cancellation failure was not retained: %#v", fault)
	}
	waitPublisherClosed(t, mixed.closed)
	select {
	case duplicate := <-observed:
		t.Fatalf("mixed publisher failure was captured more than once: %#v", duplicate)
	default:
	}

	healthy := &signalPublisher{writes: make(chan struct{}, 4), closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return healthy, nil })
	attachTestViewer(t, manager, sessionID, generation, "browser_recovered")
	if _, err := registry.Join(joinCtx, sessionID, generation, "browser_recovered"); err != nil {
		t.Fatalf("same registry did not recover after mixed failure: %v", err)
	}
	writeTestViewer(t, manager, sessionID, generation, "browser_recovered", 1)
	select {
	case <-healthy.writes:
	case <-time.After(2 * time.Second):
		t.Fatal("fresh publisher did not deliver output after mixed failure")
	}
}

func TestPublisherLeaveReportsCloseFailureAndRecovers(t *testing.T) {
	manager, sessionID, generation, registry := newPublisherTestSession(t)
	observed := make(chan errorreport.Fault, 8)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { observed <- fault })
	defer restore()
	reference := supportref.New()
	joinCtx := supportref.WithContext(context.Background(), reference)
	failed := &closeFailurePublisher{closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return failed, nil })
	attachTestViewer(t, manager, sessionID, generation, "browser_leaving")
	updates, err := registry.Join(joinCtx, sessionID, generation, "browser_leaving")
	if err != nil {
		t.Fatal(err)
	}
	<-updates
	registry.Leave(sessionID, "browser_leaving")
	fault := waitPublisherFault(t, observed)
	if fault.Operation != "sessions" || fault.Stage != lifecycleStage || fault.Code != sessionCode || fault.SupportReference != reference {
		t.Fatalf("publisher cleanup fault classification = %#v", fault)
	}
	serialized := strings.Join([]string{fault.Component, fault.Operation, fault.Stage, fault.Code, fault.Cause, fault.ErrorType, fault.SupportReference, strings.Join(fault.ErrorChain, ",")}, "|")
	if strings.Contains(serialized, "private publisher close marker") {
		t.Fatalf("fault retained private close text: %q", serialized)
	}
	waitPublisherClosed(t, failed.closed)
	select {
	case duplicate := <-observed:
		t.Fatalf("publisher cleanup failure was captured more than once: %#v", duplicate)
	default:
	}

	healthy := &signalPublisher{writes: make(chan struct{}, 4), closed: make(chan struct{})}
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return healthy, nil })
	attachTestViewer(t, manager, sessionID, generation, "browser_recovered")
	if _, err := registry.Join(joinCtx, sessionID, generation, "browser_recovered"); err != nil {
		t.Fatalf("same registry did not recover after cleanup failure: %v", err)
	}
	writeTestViewer(t, manager, sessionID, generation, "browser_recovered", 1)
	select {
	case <-healthy.writes:
	case <-time.After(2 * time.Second):
		t.Fatal("fresh publisher did not deliver output after cleanup recovery")
	}
}

type shortHeaderPublisher struct {
	calls  atomic.Int32
	closed chan struct{}
	once   sync.Once
}

func (p *shortHeaderPublisher) Write(data []byte) (int, error) {
	if p.calls.Add(1) == 1 {
		return len(data) - 1, nil
	}
	return len(data), nil
}
func (p *shortHeaderPublisher) Close() error { p.once.Do(func() { close(p.closed) }); return nil }

type deadlineResetPublisher struct {
	closed chan struct{}
	once   sync.Once
}

func (*deadlineResetPublisher) Write(data []byte) (int, error) { return len(data), nil }
func (p *deadlineResetPublisher) SetWriteDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return errors.New("private deadline reset marker")
	}
	return nil
}
func (p *deadlineResetPublisher) Close() error { p.once.Do(func() { close(p.closed) }); return nil }

type causePublisher struct {
	failure error
	closed  chan struct{}
	once    sync.Once
}

func (p *causePublisher) Write([]byte) (int, error) { return 0, p.failure }
func (p *causePublisher) Close() error              { p.once.Do(func() { close(p.closed) }); return nil }

type closeFailurePublisher struct {
	closed chan struct{}
	once   sync.Once
}

func (*closeFailurePublisher) Write(data []byte) (int, error) { return len(data), nil }
func (p *closeFailurePublisher) Close() error {
	p.once.Do(func() { close(p.closed) })
	return errors.New("private publisher close marker")
}

type signalPublisher struct {
	writes chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (p *signalPublisher) Write(data []byte) (int, error) {
	select {
	case p.writes <- struct{}{}:
	default:
	}
	return len(data), nil
}
func (p *signalPublisher) Close() error { p.once.Do(func() { close(p.closed) }); return nil }

func newPublisherTestSession(t *testing.T) (*session.Manager, string, uint64, *Registry) {
	t.Helper()
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{
		Launch:          func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) },
		MaxSessions:     1,
		MaxAttachments:  8,
		HistoryBytes:    1 << 20,
		AttachmentBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	created, err := manager.Create(ctx, session.CreateRequest{
		Name: "publisher-test",
		Command: pty.Command{
			Path:       "/bin/sh",
			Args:       []string{"-c", "stty -echo; cat"},
			CWD:        root,
			Env:        []string{"PATH=/usr/bin:/bin", "TERM=xterm"},
			Dimensions: pty.Dimensions{Columns: 80, Rows: 24},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(manager, func(context.Context) (ed25519.PrivateKey, error) { return private, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.SetPublisher(nil) })
	return manager, created.ID, created.Generation, registry
}

func attachTestViewer(t *testing.T, manager *session.Manager, sessionID string, generation uint64, attachmentID string) {
	t.Helper()
	if _, err := manager.AttachLiveParticipantAtGeneration(sessionID, session.Participant{
		AttachmentID: attachmentID,
		AccountID:    "account_" + attachmentID,
		ClientID:     attachmentID,
		Role:         "interactive",
		ConnectedAt:  time.Now(),
		Browser:      true,
	}, generation); err != nil {
		t.Fatal(err)
	}
}

func writeTestViewer(t *testing.T, manager *session.Manager, sessionID string, generation uint64, attachmentID string, sequence uint64) {
	t.Helper()
	if _, err := manager.Write(sessionID, session.InputKey{
		ClientID: attachmentID, AttachmentID: attachmentID, Generation: generation, InputSequence: sequence,
	}, []byte("browser output trigger\n")); err != nil {
		t.Fatal(err)
	}
}

func waitPublisherFault(t *testing.T, observed <-chan errorreport.Fault) errorreport.Fault {
	t.Helper()
	select {
	case fault := <-observed:
		return fault
	case <-time.After(2 * time.Second):
		t.Fatal("publisher failure was not reported")
		return errorreport.Fault{}
	}
}

func waitPublisherClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher stream was not closed")
	}
}

// The browser that joins first does so inside a request with a deadline. The shared output stream must outlive
// that request: the carrier resets a stream whose open context ends, and every browser then lost its terminal
// about 10 s after attaching (web lane, prod 13:51Z).
func TestPublisherStreamOutlivesTheJoiningRequest(t *testing.T) {
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 1, MaxAttachments: 8, HistoryBytes: 1 << 20, AttachmentBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	}()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	created, err := manager.Create(context.Background(), session.CreateRequest{Name: "shared", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "exec sleep 30"}, CWD: root, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(manager, func(context.Context) (ed25519.PrivateKey, error) { return private, nil })
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	opened := make(chan context.Context, 1)
	r.SetPublisher(func(ctx context.Context, _ string) (io.WriteCloser, error) { opened <- ctx; return writer, nil })
	browser := session.Participant{AttachmentID: "browser_a", AccountID: "account_a", ClientID: "browser_a", Role: "owner", ConnectedAt: time.Now(), Browser: true}
	if _, err := manager.AttachLiveParticipantAtGeneration(created.ID, browser, created.Generation); err != nil {
		t.Fatal(err)
	}
	request, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if _, err := r.Join(request, created.ID, created.Generation, browser.AttachmentID); err != nil {
		t.Fatal(err)
	}
	streamCtx := <-opened
	<-request.Done()
	cancel()
	time.Sleep(20 * time.Millisecond)
	if err := streamCtx.Err(); err != nil {
		t.Fatalf("the shared output stream ended with the joining request: %v", err)
	}
	r.Leave(created.ID, browser.AttachmentID)
}

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
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
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
	nextRecord := make(chan []byte, 1)
	go func() {
		var header [4]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			nextRecord <- nil
			return
		}
		n := binary.BigEndian.Uint32(header[:])
		if n == 0 || n > browserbroadcast.MaxRecordBytes {
			nextRecord <- nil
			return
		}
		raw := make([]byte, n)
		if _, err := io.ReadFull(reader, raw); err != nil {
			nextRecord <- nil
			return
		}
		nextRecord <- raw
	}()
	select {
	case raw := <-nextRecord:
		if raw == nil {
			t.Fatal("missing output after revocation")
		}
		if _, err := browserbroadcast.Open(raw, epoch, public); err == nil {
			t.Fatal("departed participant's key decrypted future output")
		}
		record, err := browserbroadcast.Open(raw, rotated, public)
		if err != nil || !strings.Contains(string(record.Data), "<after-revoke>") {
			t.Fatalf("remaining participant could not decrypt future output: %v", err)
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

type failedPublisher struct{ failure error }

func (p failedPublisher) Write([]byte) (int, error) { return 0, p.failure }
func (failedPublisher) Close() error                { return nil }

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
			Args:       []string{"-c", "sleep 0.2; printf 'browser-private-marker'; sleep 10"},
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
	secretCause := errors.New("private publisher failure: browser-private-marker")
	registry.SetPublisher(func(context.Context, string) (io.WriteCloser, error) {
		return failedPublisher{failure: secretCause}, nil
	})

	observed := make(chan errorreport.Fault, 8)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { observed <- fault })
	defer restore()
	reference := supportref.New()
	joinCtx := supportref.WithContext(context.Background(), reference)
	if _, err := registry.Join(joinCtx, created.ID, created.Generation, "browser_viewer"); err != nil {
		t.Fatal(err)
	}
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
	if !bytes.Equal(private, originalPrivate) {
		t.Fatal("publisher cleanup modified the shared signing identity")
	}
	select {
	case fault := <-observed:
		t.Fatalf("publisher failure was captured more than once: %#v", fault)
	case <-time.After(100 * time.Millisecond):
	}
}

type cyclicPublisherError struct{}

func (*cyclicPublisherError) Error() string   { return "cyclic publisher error" }
func (e *cyclicPublisherError) Unwrap() error { return e }

func TestPublisherTerminationRequiresOnlyOrdinaryLeaves(t *testing.T) {
	if !expectedPublisherEnd(errors.Join(context.Canceled, io.EOF, net.ErrClosed)) {
		t.Fatal("ordinary joined termination was not suppressed")
	}
	if expectedPublisherEnd(errors.Join(context.Canceled, errors.New("substantive publisher failure"))) {
		t.Fatal("mixed cancellation and failure was suppressed")
	}
	if expectedPublisherEnd(&cyclicPublisherError{}) {
		t.Fatal("cyclic error was treated as ordinary termination")
	}
}

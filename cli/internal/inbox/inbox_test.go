package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type fakeClient struct {
	data         []byte
	contentCalls int
	offsets      []int64
}

type interruptedClient struct {
	fakeClient
	item     filetransfer.Manifest
	cancel   context.CancelFunc
	receipts []string
}

type pendingSequenceClient struct {
	mu        sync.Mutex
	errors    []error
	calls     int
	called    chan int
	recovered chan struct{}
}

func (c *pendingSequenceClient) Pending(ctx context.Context, _ string, _ int) ([]filetransfer.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	c.called <- call
	if call <= len(c.errors) {
		return nil, c.errors[call-1]
	}
	if call == len(c.errors)+1 {
		close(c.recovered)
		return nil, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*pendingSequenceClient) Content(context.Context, filetransfer.Manifest, int64) (*http.Response, error) {
	return nil, errors.New("content request was unexpected")
}
func (*pendingSequenceClient) Receipt(context.Context, string, string, string) error { return nil }

type receiptRecoveryClient struct {
	fakeClient
	item       filetransfer.Manifest
	cancel     context.CancelFunc
	receiptErr error
	receipts   []string
}

func (c *receiptRecoveryClient) Pending(ctx context.Context, _ string, _ int) ([]filetransfer.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []filetransfer.Manifest{c.item}, nil
}

func (c *receiptRecoveryClient) Content(ctx context.Context, item filetransfer.Manifest, offset int64) (*http.Response, error) {
	return c.fakeClient.Content(ctx, item, offset)
}

func (c *receiptRecoveryClient) Receipt(_ context.Context, _ string, code, _ string) error {
	c.receipts = append(c.receipts, code)
	if len(c.receipts) == 1 {
		return c.receiptErr
	}
	c.cancel()
	return nil
}

type blockingBody struct {
	started chan struct{}
	closed  chan struct{}
	err     error
	once    sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, b.err
}

func (b *blockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

type blockedContentClient struct {
	item    filetransfer.Manifest
	body    *blockingBody
	receipt int
}

func (c *blockedContentClient) Pending(ctx context.Context, _ string, _ int) ([]filetransfer.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []filetransfer.Manifest{c.item}, nil
}

func (c *blockedContentClient) Content(context.Context, filetransfer.Manifest, int64) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: c.body, Header: make(http.Header)}, nil
}

func (c *blockedContentClient) Receipt(context.Context, string, string, string) error {
	c.receipt++
	return nil
}

type contentOpenFailureClient struct {
	item       filetransfer.Manifest
	cancel     context.CancelFunc
	contentErr error
	receipt    string
}

func (c *contentOpenFailureClient) Pending(ctx context.Context, _ string, _ int) ([]filetransfer.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []filetransfer.Manifest{c.item}, nil
}

func (c *contentOpenFailureClient) Content(context.Context, filetransfer.Manifest, int64) (*http.Response, error) {
	return nil, c.contentErr
}

func (c *contentOpenFailureClient) Receipt(_ context.Context, _, code, _ string) error {
	c.receipt = code
	c.cancel()
	return nil
}

func (c *interruptedClient) Pending(ctx context.Context, _ string, _ int) ([]filetransfer.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []filetransfer.Manifest{c.item}, nil
}

func (c *interruptedClient) Content(ctx context.Context, item filetransfer.Manifest, offset int64) (*http.Response, error) {
	response, err := c.fakeClient.Content(ctx, item, offset)
	if len(c.offsets) == 1 {
		response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(c.data[:3]), failureReader{}))
	}
	return response, err
}

type failureReader struct{}

func (failureReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func (c *interruptedClient) Receipt(_ context.Context, _, code, _ string) error {
	c.receipts = append(c.receipts, code)
	c.cancel()
	return nil
}

func TestRunResumesInterruptedDownloadBeforeSendingReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	data := []byte("resume these bytes")
	client := &interruptedClient{fakeClient: fakeClient{data: data}, item: manifest("ft_interrupted", "file.bin", data), cancel: cancel}
	root := filepath.Join(t.TempDir(), "Paperboat Inbox")
	receiver, err := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if len(client.receipts) != 1 || client.receipts[0] != "stored" {
		t.Fatalf("interruption produced terminal receipt: %v", client.receipts)
	}
	if len(client.offsets) != 2 || client.offsets[1] != 3 {
		t.Fatalf("resume offsets: %v", client.offsets)
	}
	got, err := os.ReadFile(filepath.Join(root, "file.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("published content mismatch: %v", err)
	}
}

func TestRunObservesPendingFailuresOnceAndRecoversWithTheSameReference(t *testing.T) {
	private := errors.New("private credential response and machine label")
	client := &pendingSequenceClient{
		errors: []error{private, private}, called: make(chan int, 8), recovered: make(chan struct{}),
	}
	receiver, err := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(t.TempDir(), "Paperboat Inbox")})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	recorder := diagnostics.NewMemoryRecorder()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := diagnostics.WithRecorder(supportref.WithContext(base, reference), recorder)
	faults := make(chan errorreport.Fault, 4)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	done := make(chan error, 1)
	go func() { done <- receiver.Run(ctx) }()

	select {
	case call := <-client.called:
		if call != 1 {
			t.Fatalf("first pending call = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not poll pending transfers")
	}
	var outage errorreport.Fault
	select {
	case outage = <-faults:
	case <-time.After(time.Second):
		t.Fatal("pending outage was not observed")
	}
	if outage.Stage != "control_request" || outage.Code != "control_request_failed" || outage.SupportReference != reference || outage.Outcome != "failed" {
		t.Fatalf("pending fault = %#v", outage)
	}
	if strings.Contains(strings.Join(outage.ErrorChain, ","), "private") || strings.Contains(outage.Cause, "private") {
		t.Fatalf("private response reached diagnostics: %#v", outage)
	}
	for want := 2; want <= 4; want++ {
		select {
		case call := <-client.called:
			if call != want {
				t.Fatalf("pending call = %d, want %d", call, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Run did not reach pending call %d", want)
		}
	}
	select {
	case <-client.recovered:
	case <-time.After(2 * time.Second):
		t.Fatal("pending service did not recover")
	}
	if len(faults) != 0 {
		t.Fatalf("unchanged pending failures were emitted more than once: %d", len(faults))
	}
	waitInboxRecovery(t, recorder, outage.Stage, reference)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run after recovery: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunObservesReceiptFailureAndRecoversWithoutResponseDetails(t *testing.T) {
	data := []byte("private file payload")
	base, cancel := context.WithCancel(context.Background())
	client := &receiptRecoveryClient{
		fakeClient: fakeClient{data: data}, item: manifest("ft_receipt", "result.bin", data), cancel: cancel,
		receiptErr: &filetransfer.Error{Code: "storage_unavailable", Message: "private receipt body", StatusCode: http.StatusServiceUnavailable},
	}
	receiver, err := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(t.TempDir(), "Paperboat Inbox")})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	reference := supportref.New()
	recorder := diagnostics.NewMemoryRecorder()
	ctx := diagnostics.WithRecorder(supportref.WithContext(base, reference), recorder)
	faults := make(chan errorreport.Fault, 2)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	done := make(chan error, 1)
	go func() { done <- receiver.Run(ctx) }()

	var failure errorreport.Fault
	select {
	case failure = <-faults:
	case <-time.After(2 * time.Second):
		t.Fatal("receipt failure was not observed")
	}
	if failure.Stage != "control_request" || failure.Code != "file_transfer_failed" || failure.HTTPStatus != http.StatusServiceUnavailable || failure.SupportReference != reference {
		t.Fatalf("receipt fault = %#v", failure)
	}
	if strings.Contains(strings.Join(failure.ErrorChain, ","), "private") || strings.Contains(failure.Cause, "private") {
		t.Fatalf("private receipt details reached diagnostics: %#v", failure)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run after receipt recovery: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run did not finish after receipt recovery")
	}
	if len(client.receipts) != 2 || client.receipts[0] != "stored" || client.receipts[1] != "stored" || client.contentCalls != 1 {
		t.Fatalf("receipt retry changed delivery behavior: receipts=%v content_calls=%d", client.receipts, client.contentCalls)
	}
	if len(faults) != 0 {
		t.Fatalf("receipt recovery produced extra failures: %d", len(faults))
	}
	waitInboxRecovery(t, recorder, failure.Stage, reference)
}

func TestRunClosesBlockedContentOnCancellationAndReturnsReadCause(t *testing.T) {
	readFailure := errors.New("private stream read failure")
	body := &blockingBody{started: make(chan struct{}), closed: make(chan struct{}), err: readFailure}
	client := &blockedContentClient{item: manifest("ft_blocked", "result.bin", []byte("expected data")), body: body}
	receiver, err := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(t.TempDir(), "Paperboat Inbox")})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	recorder := diagnostics.NewMemoryRecorder()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := diagnostics.WithRecorder(supportref.WithContext(base, reference), recorder)
	faults := make(chan errorreport.Fault, 2)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	done := make(chan error, 1)
	go func() { done <- receiver.Run(ctx) }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("content reader did not block")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, readFailure) {
			t.Fatalf("Run lost concurrent cancellation/read causes: %v", err)
		}
		if strings.Contains(err.Error(), "private stream") {
			t.Fatalf("Run error exposed the read error text: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the content body and stop Run")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("content body was not closed")
	}
	select {
	case fault := <-faults:
		if fault.Stage != "delivery" || fault.Code != "file_transfer_failed" || fault.SupportReference != reference {
			t.Fatalf("canceled delivery fault = %#v", fault)
		}
		if strings.Contains(strings.Join(fault.ErrorChain, ","), "private") || strings.Contains(fault.Cause, "private") {
			t.Fatalf("private stream details reached diagnostics: %#v", fault)
		}
	default:
		t.Fatal("concurrent stream read failure was not observed")
	}
	if client.receipt != 0 {
		t.Fatalf("canceled stream received a terminal receipt: %d", client.receipt)
	}
}

func TestRunClassifiesContentOpenFailureAndSendsBoundedReceipt(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	client := &contentOpenFailureClient{
		item: manifest("ft_open", "result.bin", []byte("content")), cancel: cancel,
		contentErr: &filetransfer.Error{Code: "digest_mismatch", Message: "private open response", StatusCode: http.StatusBadRequest},
	}
	receiver, err := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(t.TempDir(), "Paperboat Inbox")})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	reference := supportref.New()
	faults := make(chan errorreport.Fault, 2)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	done := make(chan error, 1)
	go func() {
		done <- receiver.Run(diagnostics.WithRecorder(supportref.WithContext(base, reference), diagnostics.NewMemoryRecorder()))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run after open failure: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run did not stop after open failure receipt")
	}
	if client.receipt != "digest_mismatch" {
		t.Fatalf("receipt code = %q", client.receipt)
	}
	select {
	case fault := <-faults:
		if fault.Stage != "stream_open" || fault.Code != "file_transfer_failed" || fault.HTTPStatus != http.StatusBadRequest || fault.SupportReference != reference {
			t.Fatalf("content open fault = %#v", fault)
		}
		if strings.Contains(strings.Join(fault.ErrorChain, ","), "private") || strings.Contains(fault.Cause, "private") {
			t.Fatalf("private open details reached diagnostics: %#v", fault)
		}
	default:
		t.Fatal("content open failure was not observed")
	}
}

func TestStorageErrorsRetainCauseWithoutFormattingLocalPath(t *testing.T) {
	privatePath := filepath.Join(t.TempDir(), "private-user", "token.txt")
	original := fmt.Errorf("write %s: %w", privatePath, os.ErrPermission)
	err := storageError(original)
	if !errors.Is(err, os.ErrPermission) || !errors.Is(err, errStorageUnavailable) || errorCode(err) != "storage_unavailable" {
		t.Fatalf("storage classification lost its original cause: %v", err)
	}
	if strings.Contains(err.Error(), privatePath) || strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("storage error exposed local path or raw cause: %v", err)
	}
	var staged interface{ DiagnosticStage() string }
	var coded interface{ DiagnosticCode() string }
	if !errors.As(err, &staged) || staged.DiagnosticStage() != "delivery" || !errors.As(err, &coded) || coded.DiagnosticCode() != "file_transfer_failed" {
		t.Fatalf("storage error lacks typed delivery phase: %T", err)
	}
}

func TestErrorCodeUsesTypedClassesAndDoesNotParseMessages(t *testing.T) {
	if got := errorCode(errors.New("private text contains digest_mismatch")); got != "storage_unavailable" {
		t.Fatalf("message-sniffed code = %q", got)
	}
	if got := errorCode(errDigestMismatch); got != "digest_mismatch" {
		t.Fatalf("typed local code = %q", got)
	}
	if got := errorCode(&filetransfer.Error{Code: "recipient_unavailable"}); got != "recipient_unavailable" {
		t.Fatalf("typed transfer result code = %q", got)
	}
	if got := errorCode(&filetransfer.Error{Code: "private_error_code"}); got != "storage_unavailable" {
		t.Fatalf("unbounded transfer code = %q", got)
	}
	if got := errorCode(errors.Join(errDigestMismatch, os.ErrPermission)); got != "storage_unavailable" {
		t.Fatalf("mixed failure was hidden by a product code: %q", got)
	}
}

func waitInboxRecovery(t *testing.T, recorder *diagnostics.Recorder, stage, reference string) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, event := range recorder.Recent() {
			if event.Code == "recovered" && event.Category == stage && event.SupportReference == reference {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("recovery event missing for stage=%s reference=%s", stage, reference)
		case <-ticker.C:
		}
	}
}

func (f *fakeClient) Pending(context.Context, string, int) ([]filetransfer.Manifest, error) {
	return nil, nil
}
func (f *fakeClient) Receipt(context.Context, string, string, string) error { return nil }
func (f *fakeClient) Content(_ context.Context, _ filetransfer.Manifest, offset int64) (*http.Response, error) {
	f.contentCalls++
	f.offsets = append(f.offsets, offset)
	status := http.StatusOK
	if offset > 0 {
		status = http.StatusPartialContent
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(f.data[offset:])), Header: make(http.Header)}, nil
}

func manifest(id, name string, data []byte) filetransfer.Manifest {
	digest := sha256.Sum256(data)
	return filetransfer.Manifest{TransferID: id, BatchID: "batch_1", SourceMachineID: "machine_host", DestinationMachineID: "machine_local", InitiatingUserID: "user_1", SessionID: "session_1", Basename: name, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), State: "pending"}
}

func TestDeliverResumesPartialAcrossPBRestartAndReturnsDurableRelativePath(t *testing.T) {
	downloads := t.TempDir()
	data := []byte("exact transfer bytes")
	client := &fakeClient{data: data}
	_, err := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(downloads, "Paperboat Inbox")})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(downloads, "Paperboat Inbox")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".paperboat-transfer-ft_resume.part"), data[:6], 0o600); err != nil {
		t.Fatal(err)
	}
	// A new Inbox has no process-local state from the instance that wrote the partial.
	receiver, err := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(downloads, "Paperboat Inbox")})
	if err != nil {
		t.Fatal(err)
	}
	path, err := receiver.Deliver(context.Background(), manifest("ft_resume", "result.bin", data))
	if err != nil {
		t.Fatal(err)
	}
	if path != "Paperboat Inbox/result.bin" || len(client.offsets) != 1 || client.offsets[0] != 6 {
		t.Fatalf("path=%q offsets=%v", path, client.offsets)
	}
	stored, err := os.ReadFile(filepath.Join(downloads, filepath.FromSlash(path)))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("stored=%q err=%v", stored, err)
	}
}

func TestDeliverUsesCollisionNameAndDeduplicatesTransfer(t *testing.T) {
	downloads := t.TempDir()
	root := filepath.Join(downloads, "Paperboat Inbox")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.txt"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := []byte("new")
	client := &fakeClient{data: data}
	receiver, _ := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(downloads, "Paperboat Inbox")})
	item := manifest("ft_duplicate", "report.txt", data)
	first, err := receiver.Deliver(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	second, err := receiver.Deliver(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if first != "Paperboat Inbox/report (2).txt" || second != first || client.contentCalls != 1 {
		t.Fatalf("first=%q second=%q calls=%d", first, second, client.contentCalls)
	}
}

func TestDeliverMixedTenFileBatchPreservesExactBytesWithoutDuplicates(t *testing.T) {
	downloads := t.TempDir()
	contents := [][]byte{
		nil,
		[]byte("plain text\nwith newline"),
		{0x00, 0xff, 0x01, 0x80},
		[]byte("{\"json\":true}"),
		[]byte("no extension"),
		[]byte("unicode contents"),
		bytes.Repeat([]byte{0xa5}, 32<<10),
		[]byte("collision one"),
		[]byte("collision two"),
		[]byte("final file"),
	}
	names := []string{"empty", "notes.txt", "opaque.bin", "data.json", "README", "résumé 最終.txt", "chunk.dat", "duplicate.txt", "duplicate.txt", "archive.tar.gz"}
	paths := make([]string, len(contents))
	for index, content := range contents {
		client := &fakeClient{data: content}
		receiver, err := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(downloads, "Paperboat Inbox")})
		if err != nil {
			t.Fatal(err)
		}
		item := manifest(fmt.Sprintf("ft_mixed_%d", index), names[index], content)
		paths[index], err = receiver.Deliver(context.Background(), item)
		if err != nil {
			t.Fatalf("deliver %d: %v", index, err)
		}
		again, err := receiver.Deliver(context.Background(), item)
		if err != nil || again != paths[index] {
			t.Fatalf("replay %d path=%q err=%v", index, again, err)
		}
		stored, err := os.ReadFile(filepath.Join(downloads, filepath.FromSlash(paths[index])))
		if err != nil || !bytes.Equal(stored, content) {
			t.Fatalf("stored %d differs: size=%d err=%v", index, len(stored), err)
		}
	}
	if paths[7] != "Paperboat Inbox/duplicate.txt" || paths[8] != "Paperboat Inbox/duplicate (2).txt" {
		t.Fatalf("collision paths=%q, %q", paths[7], paths[8])
	}
	entries, err := os.ReadDir(filepath.Join(downloads, "Paperboat Inbox"))
	if err != nil {
		t.Fatal(err)
	}
	visible := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			visible++
		}
	}
	if visible != len(contents) {
		t.Fatalf("visible files=%d want=%d", visible, len(contents))
	}
}

func TestDeliverRejectsAlteredJournaledFile(t *testing.T) {
	downloads := t.TempDir()
	data := []byte("original")
	client := &fakeClient{data: data}
	receiver, _ := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(downloads, "Paperboat Inbox")})
	item := manifest("ft_altered", "report.txt", data)
	path, err := receiver.Deliver(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloads, filepath.FromSlash(path)), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Deliver(context.Background(), item); err == nil || errorCode(err) != "digest_mismatch" {
		t.Fatalf("err=%v", err)
	}
	if client.contentCalls != 1 {
		t.Fatalf("content calls=%d", client.contentCalls)
	}
}

func TestDeliverRecoversJournalBeforeLinkCrash(t *testing.T) {
	downloads := t.TempDir()
	root := filepath.Join(downloads, "Paperboat Inbox")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("recovered")
	item := manifest("ft_crash", "result.bin", data)
	receipts := journal{Version: 1, Entries: map[string]receipt{item.TransferID: {Digest: item.SHA256, Path: "Paperboat Inbox/result.bin", At: time.Now().UTC()}}}
	if err := saveJournal(root, receipts); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{data: data}
	receiver, _ := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(downloads, "Paperboat Inbox")})
	path, err := receiver.Deliver(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if path != "Paperboat Inbox/result.bin" {
		t.Fatalf("path=%q", path)
	}
	stored, err := os.ReadFile(filepath.Join(downloads, filepath.FromSlash(path)))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("stored=%q err=%v", stored, err)
	}
}

func TestDeliverSupportsEmptyFileWithoutDownload(t *testing.T) {
	client := &fakeClient{}
	downloads := t.TempDir()
	receiver, _ := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(downloads, "Paperboat Inbox")})
	path, err := receiver.Deliver(context.Background(), manifest("ft_empty", "empty", nil))
	if err != nil {
		t.Fatal(err)
	}
	if client.contentCalls != 0 || path != "Paperboat Inbox/empty" {
		t.Fatalf("calls=%d path=%q", client.contentCalls, path)
	}
	info, err := os.Stat(filepath.Join(downloads, filepath.FromSlash(path)))
	if err != nil || info.Size() != 0 {
		t.Fatalf("info=%v err=%v", info, err)
	}
}

func TestDeliverRejectsDigestMismatchAndRemovesPartial(t *testing.T) {
	downloads := t.TempDir()
	client := &fakeClient{data: []byte("wrong")}
	receiver, _ := New(Config{Client: client, MachineID: "machine_local", SessionID: "session_1", Path: filepath.Join(downloads, "Paperboat Inbox")})
	item := manifest("ft_bad", "bad.bin", []byte("right"))
	if _, err := receiver.Deliver(context.Background(), item); err == nil || errorCode(err) != "digest_mismatch" {
		t.Fatalf("err=%v", err)
	}
	partial := filepath.Join(downloads, "Paperboat Inbox", ".paperboat-transfer-ft_bad.part")
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial remains: %v", err)
	}
}

func TestLocalBasenamePreservesUnicodeAndAdaptsWindowsReservedNames(t *testing.T) {
	if got := localBasename("résumé 最終.txt", "windows"); got != "résumé 最終.txt" {
		t.Fatalf("unicode name = %q", got)
	}
	for input, want := range map[string]string{
		"CON":              "_CON",
		"com1.log":         "_com1.log",
		"report:final.txt": "report_final.txt",
		"trailing. ":       "trailing",
	} {
		if got := localBasename(input, "windows"); got != want {
			t.Errorf("localBasename(%q) = %q, want %q", input, got, want)
		}
	}
	if got := localBasename("CON", "darwin"); got != "CON" {
		t.Fatalf("darwin name = %q", got)
	}
}

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clienttransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

// transferGeneration is one runtime worker generation: its own transfer service and native dispatch over the
// durable store and files both generations share, as two worker processes on one machine do.
type transferGeneration struct {
	dispatch *NativeDispatchHandler
	client   *NativeDispatchClient
	opens    atomic.Int64
	mu       sync.Mutex
	requests []string // method and path of every request this generation served
}

func (g *transferGeneration) served() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...)
}

func newTransferGeneration(t *testing.T, durable *store.Store, root string, revoked <-chan struct{}) *transferGeneration {
	t.Helper()
	policy := filetransfer.DefaultPolicy
	policy.AutomaticMaxBatchBytes = 200 << 20
	service, err := filetransfer.New(filetransfer.Config{Root: filepath.Join(root, "files"), LocalMachineID: "machine_host", Store: durable, Policy: filetransfer.NewPolicyStore(policy),
		PublishRoot: func() (string, error) { return filepath.Join(root, "inbox"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := operation.NewJournal(32)
	transfers, err := NewNativeFileTransferHandler(FileTransferHandlerConfig{Service: service, Journal: journal, Authorizer: func(token string) (Authorizer, error) {
		if token != "token" {
			return nil, errors.New("invalid")
		}
		return authorizerFunc(func(context.Context, protocol.Frame) (Authorization, error) {
			return Authorization{JournalBinding: "env:1:cli:1", EnvironmentID: "env_1", MachineID: "machine_host", SourceMachineID: "machine_client", UserID: "user_1", ClientID: "cli_1", SessionID: "ses_1", RevokedSignal: revoked}, nil
		}), nil
	}, AuthorizeCreate: func(authorization Authorization, request CreateFileTransferRequest) bool {
		return request.DestinationMachineID == authorization.MachineID && request.InitiatingUserID == authorization.UserID
	}})
	if err != nil {
		t.Fatal(err)
	}
	generation := &transferGeneration{}
	recorded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generation.mu.Lock()
		generation.requests = append(generation.requests, r.Method+" "+r.URL.Path)
		generation.mu.Unlock()
		transfers.ServeHTTP(w, r)
	})
	never := make(chan struct{})
	generation.dispatch, err = NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 8, HandshakeTimeout: time.Second,
		Fence:     func(context.Context) (func(), <-chan struct{}, error) { return func() {}, never, nil },
		Authorize: func(context.Context, streamauth.Header) (string, error) { return "access", nil },
		ServeStream: func(ctx context.Context, _ streamauth.Header, conn net.Conn) error {
			return ServeHTTPConnection(ctx, conn, recorded)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := httptest.NewServer(generation.dispatch)
	t.Cleanup(worker.Close)
	t.Cleanup(func() { _ = generation.dispatch.Close(context.Background()) })
	generation.client, err = NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

// accepting stands for the runtime listener both generations share: a new stream reaches the generation that
// accepts on it, the next one once the retiring one stopped accepting.
type acceptingGeneration struct {
	current atomic.Pointer[transferGeneration]
}

func (a *acceptingGeneration) open(ctx context.Context) (net.Conn, error) {
	generation := a.current.Load()
	generation.opens.Add(1)
	header, err := streamauth.New("operation", "file_transfer", "stream", "allowed", time.Now().Add(time.Minute), 1<<30)
	if err != nil {
		return nil, err
	}
	return generation.client.Open(ctx, header)
}

func drainTestClient(t *testing.T, accepting *acceptingGeneration) *clienttransfer.NativeClient {
	t.Helper()
	client, err := clienttransfer.NewNativeClient("http://helper.test/v1/file-transfers", clienttransfer.Auth{Token: "token"}, clienttransfer.Binding{SourceMachineID: "machine_client", DestinationMachineID: "machine_host", InitiatingUserID: "user_1"}, accepting.open)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func drainTestSource(name string, data []byte) clienttransfer.Source {
	return clienttransfer.Source{Basename: name, Size: int64(len(data)), SHA256: sha256.Sum256(data), Reader: bytes.NewReader(data)}
}

// retire is what a retiring worker does after the next generation took the listener: DrainLocal, then Shutdown.
func retire(generation *transferGeneration) <-chan error {
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := generation.dispatch.DrainTransfers(ctx)
		drained <- errors.Join(err, generation.dispatch.Close(ctx))
	}()
	return drained
}

// A pb send in flight during an update finishes on the generation that holds it, byte-exact and without a
// reconnect or resume (pbvm4: +2.95 s while the old worker cut its stream at Shutdown). A send started
// mid-update lands on the new generation, and the old one finishes once its last transfer ended.
func TestRetiringGenerationDrainsInFlightTransfer(t *testing.T) {
	root := t.TempDir()
	durable, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	old, next := newTransferGeneration(t, durable, root, nil), newTransferGeneration(t, durable, root, nil)
	accepting := &acceptingGeneration{}
	accepting.current.Store(old)

	data := make([]byte, 100<<20)
	rand.New(rand.NewSource(1)).Read(data)
	inFlight := drainTestClient(t, accepting)
	handover := make(chan struct{})
	var once sync.Once
	inFlight.Committed = func(_ string, committed int64) {
		if committed >= 10<<20 {
			once.Do(func() { close(handover) })
		}
	}
	type result struct {
		batch clienttransfer.Batch
		err   error
	}
	sent := make(chan result, 1)
	go func() {
		batch, err := inFlight.SendBatch(t.Context(), "batch_in_flight", "ses_1", []clienttransfer.Source{drainTestSource("big.bin", data)})
		sent <- result{batch, err}
	}()
	select {
	case <-handover:
	case <-time.After(30 * time.Second):
		t.Fatal("the transfer never got under way")
	}
	accepting.current.Store(next)
	drained := retire(old)

	fresh := drainTestClient(t, accepting)
	small := []byte("sent during the update")
	smallBatch, err := fresh.SendBatch(t.Context(), "batch_mid_update", "ses_1", []clienttransfer.Source{drainTestSource("small.txt", small)})
	if err != nil {
		t.Fatalf("a send started mid-update failed: %v", err)
	}
	got := <-sent
	if got.err != nil {
		t.Fatalf("the in-flight send failed across the handover: %v", got.err)
	}
	published, err := os.Open(got.batch.Paths[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	_, err = io.Copy(digest, published)
	published.Close()
	if err != nil || [sha256.Size]byte(digest.Sum(nil)) != sha256.Sum256(data) {
		t.Fatalf("the published file differs from the one sent (err=%v)", err)
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retiring generation still runs after its last transfer ended")
	}

	bigID, smallID := got.batch.Transfers[0].TransferID, smallBatch.Transfers[0].TransferID
	creates, heads := 0, 0
	for _, request := range old.served() {
		// One HEAD reads the starting offset; another means the transfer was cut and resumed.
		if strings.HasPrefix(request, http.MethodHead) {
			if heads++; heads > 1 {
				t.Fatalf("the in-flight transfer was resumed: %s", request)
			}
		}
		if strings.Contains(request, smallID) || request == http.MethodPost+" /v1/file-transfers" && creates > 0 {
			t.Fatalf("a send started mid-update reached the retiring generation: %s", request)
		}
		if request == http.MethodPost+" /v1/file-transfers" {
			creates++
		}
	}
	if !slices.Contains(next.served(), http.MethodPatch+" /v1/file-transfers/"+smallID+"/content") {
		t.Fatal("the send started mid-update never reached the new generation")
	}
	for _, request := range next.served() {
		if strings.Contains(request, bigID+"/content") {
			t.Fatalf("the in-flight transfer moved to the new generation: %s", request)
		}
	}
	if opens := old.opens.Load(); opens != 1 {
		t.Fatalf("the in-flight send reconnected: %d streams to the retiring generation", opens)
	}
}

// blockingSource serves its first chunk and then waits, so the transfer stays unfinished on its connection.
type blockingSource struct {
	*bytes.Reader
	release chan struct{}
}

func (s blockingSource) Read(p []byte) (int, error) {
	if s.Reader.Size()-int64(s.Reader.Len()) >= protocol.FileTransferChunkBytes {
		<-s.release
		return 0, io.ErrUnexpectedEOF
	}
	return s.Reader.Read(p)
}

// Revocation still ends a transfer at once while its generation drains it.
func TestRevocationEndsDrainedTransfer(t *testing.T) {
	root := t.TempDir()
	durable, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	revoked := make(chan struct{})
	old := newTransferGeneration(t, durable, root, revoked)
	accepting := &acceptingGeneration{}
	accepting.current.Store(old)
	data := bytes.Repeat([]byte("r"), 3*protocol.FileTransferChunkBytes)
	source := blockingSource{Reader: bytes.NewReader(data), release: make(chan struct{})}
	defer close(source.release)
	client := drainTestClient(t, accepting)
	committed := make(chan struct{}, 4)
	client.Committed = func(string, int64) { committed <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_, _ = client.SendBatch(ctx, "batch_revoked", "ses_1", []clienttransfer.Source{{Basename: "held.bin", Size: int64(len(data)), SHA256: sha256.Sum256(data), Reader: source}})
	}()
	select {
	case <-committed:
	case <-time.After(10 * time.Second):
		t.Fatal("the first chunk never committed")
	}
	drained := retire(old)
	select {
	case err := <-drained:
		t.Fatalf("the retiring generation dropped an unfinished transfer (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(revoked)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("revocation did not end the drained transfer")
	}
}

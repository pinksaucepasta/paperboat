package native_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	clienttransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	hosttransfer "github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	hostserver "github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
)

// This fixture owns only endpoint storage. The relay receives encrypted packets;
// both directions use the production HTTP application handler above native QUIC.
func TestNativeTransferDurableBidirectionalResume(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	for _, relayed := range []bool{false, true} {
		name := "direct"
		if relayed {
			name = "derp_quic"
		}
		t.Run(name, func(t *testing.T) { testNativeTransferResume(t, relayed) })
	}
}

func testNativeTransferResume(t *testing.T, relayed bool) {
	t.Setenv("IN_TS_TEST", "true")
	if relayed {
		t.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	signerPublic, signerPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, clientFingerprint := testTLS(t, "transfer-cli")
	serverTLS, serverFingerprint := testTLS(t, "transfer-machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_transfer", EndpointID: "cli_transfer", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::71"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_transfer", EndpointID: "machine_transfer", Role: "machine", MachineID: "machine_transfer", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::72"}
	var clientAuthority, serverAuthority *tailnet.Authority
	directTraffic := &transferDirectTraffic{}
	if relayed {
		clientAuthority = testAuthority(t, clientBinding, testKeys{"native_test": signerPublic}, &regionalDirectBlock{})
		serverAuthority = testAuthority(t, serverBinding, testKeys{"native_test": signerPublic}, &regionalDirectBlock{})
	} else {
		clientAuthority = testAuthority(t, clientBinding, testKeys{"native_test": signerPublic}, directTraffic)
		serverAuthority = testAuthority(t, serverBinding, testKeys{"native_test": signerPublic}, directTraffic)
	}
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().UTC().Truncate(time.Second)
	clientConfig := testConfiguration(now.Unix(), 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now.Unix(), 1, serverBinding, clientBinding, "accept")
	// Freeze this fixture to its consumed application scope; other product tests
	// may extend the shared configuration helper independently.
	clientConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "file_transfer", Direction: "dial", Port: tailnet.NetworkPort, ExpiresAt: clientConfig.ExpiresAt}}
	serverConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "file_transfer", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: serverConfig.ExpiresAt}}
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	var region *tailcfg.DERPRegion
	var relay *derpquic.Server
	if relayed {
		relayTLS, _ := testTLS(t, "localhost")
		certificate, parseErr := x509.ParseCertificate(relayTLS.Certificates[0].Certificate[0])
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		roots := x509.NewCertPool()
		roots.AddCert(certificate)
		socket, listenErr := net.ListenPacket("udp4", "127.0.0.1:0")
		if listenErr != nil {
			t.Fatal(listenErr)
		}
		defer socket.Close()
		relay, err = derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: "relay_transfer", NodeGeneration: 1, ProcessEpoch: "epoch_transfer", Keys: map[string]ed25519.PublicKey{"native_test": signerPublic}})
		if err != nil {
			t.Fatal(err)
		}
		relayCtx, stopRelay := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- relay.Serve(relayCtx, socket, relayTLS) }()
		defer func() {
			stopRelay()
			_ = relay.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("transfer relay did not stop")
			}
			relay.Wait()
		}()
		node := tailnet.RegionalNode{NodeID: "relay_transfer", NodeGeneration: 1, ProcessEpoch: "epoch_transfer", Region: "test", FailureDomain: "test-a", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(socket.LocalAddr().(*net.UDPAddr).Port), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}
		applyRelayAuthority(t, clientAuthority, signerPrivate, clientConfig, clientTLS, node)
		_ = applyConfiguredRelay(t, clientAuthority, clientTLS, roots, node)
		applyRelayAuthority(t, serverAuthority, signerPrivate, serverConfig, serverTLS, node)
		region = applyConfiguredRelay(t, serverAuthority, serverTLS, roots, node)
	} else {
		region = integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1").Regions[1]
	}
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()

	root := t.TempDir()
	var durable *store.Store
	var service *hosttransfer.Service
	var handler atomic.Pointer[hostserver.FileTransferHandler]
	openStorage := func() {
		var openErr error
		durable, openErr = store.Open(ctx, store.Config{Root: filepath.Join(root, "state")})
		if openErr != nil {
			t.Fatal(openErr)
		}
		service, openErr = hosttransfer.New(hosttransfer.Config{Root: filepath.Join(root, "spool"), PublishRoot: filepath.Join(root, "published"), LocalMachineID: "machine_transfer", Store: durable})
		if openErr != nil {
			t.Fatal(openErr)
		}
		journal, journalErr := operation.NewJournal(32)
		if journalErr != nil {
			t.Fatal(journalErr)
		}
		h, handlerErr := hostserver.NewNativeFileTransferHandler(hostserver.FileTransferHandlerConfig{Service: service, Journal: journal, Authorizer: func(token string) (hostserver.Authorizer, error) {
			if token != "file-token" {
				return nil, errors.New("credential rejected")
			}
			return sliceAuthorizer(func(context.Context, protocol.Frame) (hostserver.Authorization, error) {
				return hostserver.Authorization{JournalBinding: "transfer-fixture", EnvironmentID: "env_transfer", MachineID: "machine_transfer", SourceMachineID: "cli_transfer", UserID: "account_transfer", ClientID: "cli_transfer", SessionID: "session_transfer"}, nil
			}), nil
		}, AuthorizeCreate: func(a hostserver.Authorization, r hostserver.CreateFileTransferRequest) bool {
			return r.SourceMachineID == a.SourceMachineID && r.DestinationMachineID == a.MachineID && r.InitiatingUserID == a.UserID
		}, Owns: func(a hostserver.Authorization, r store.FileTransfer) bool {
			return r.InitiatingUserID == a.UserID && r.SessionID == a.SessionID && (r.SourceMachineID == a.SourceMachineID || r.DeliveryClientID == a.ClientID)
		}})
		if handlerErr != nil {
			t.Fatal(handlerErr)
		}
		handler.Store(h)
	}
	openStorage()
	defer func() { _ = durable.Close() }()
	var rangeSeen atomic.Bool
	app, err := nativesession.New(nativesession.Config{Authorize: func(_ context.Context, h streamauth.Header) (string, error) {
		if h.Credential != "file-token" || h.Consumer != "file_transfer" {
			return "", errors.New("credential rejected")
		}
		return "grant_test", nil
	}, ServeStream: func(context.Context, streamauth.Header, net.Conn) error {
		return errors.New("unexpected non-transfer stream")
	}, ServeTransfer: func(ctx context.Context, c net.Conn) error {
		return hostserver.ServeHTTPConnection(ctx, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") == "bytes=17-" {
				rangeSeen.Store(true)
			}
			handler.Load().ServeHTTP(w, r)
		}))
	}})
	if err != nil {
		t.Fatal(err)
	}
	udp, err := serverAuthority.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	listenDone := make(chan error, 1)
	go func() { listenDone <- serverOwner.Listen(ctx, region, app.Serve) }()
	defer func() {
		cancel()
		_ = serverOwner.Close()
		select {
		case <-listenDone:
		case <-time.After(2 * time.Second):
			t.Error("native listener did not stop")
		}
	}()
	var sessionMu sync.Mutex
	var session *native.Session
	var streamID atomic.Uint64
	open := func(ctx context.Context) (net.Conn, error) {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if session == nil {
			var dialErr error
			session, dialErr = clientOwner.Dial(ctx, udp.Address(), "machine_transfer", peerquic.ClassInteractive)
			if dialErr != nil {
				return nil, dialErr
			}
		}
		h, e := streamauth.New("operation_transfer", "file_transfer", fmt.Sprintf("stream_transfer_%d", streamID.Add(1)), "file-token", now.Add(time.Minute), 4<<20)
		if e != nil {
			return nil, e
		}
		return session.OpenAuthorized(ctx, h, "grant_test", "file_transfer")
	}
	reconnect := func() {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if session != nil {
			_ = session.Close()
			session = nil
		}
	}
	defer reconnect()
	newClient := func(token string) *clienttransfer.NativeClient {
		c, e := clienttransfer.NewNativeClient("https://transfer.test/v1/file-transfers", clienttransfer.Auth{Token: token, ExpiresAt: now.Add(time.Minute)}, clienttransfer.Binding{SourceMachineID: "cli_transfer", DestinationMachineID: "machine_transfer", InitiatingUserID: "account_transfer"}, open)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	client := newClient("file-token")
	transferStarted := time.Now()
	data := bytes.Repeat([]byte("native-transfer-integrity\x00"), 128)
	sum := sha256.Sum256(data)
	payload, _ := json.Marshal(map[string]any{"batch_id": "batch_upload", "source_machine_id": "cli_transfer", "destination_machine_id": "machine_transfer", "initiating_user_id": "account_transfer", "session_id": "session_transfer", "files": []map[string]any{{"basename": "upload.bin", "size": len(data), "sha256": hex.EncodeToString(sum[:])}}})
	request := func(method, path string, body []byte, headers map[string]string) *http.Response {
		r, e := http.NewRequestWithContext(ctx, method, client.Endpoint+path, bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Authorization", "Bearer file-token")
		r.Header.Set("X-Paperboat-Request-ID", "request_transfer")
		r.Header.Set("X-Paperboat-Operation-ID", "operation_transfer_create")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		response, e := client.HTTPClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			defer response.Body.Close()
			t.Fatalf("%s %s status=%d", method, path, response.StatusCode)
		}
		return response
	}
	response := request(http.MethodPost, "", payload, map[string]string{"Content-Type": "application/json"})
	var initial clienttransfer.Batch
	if err = json.NewDecoder(response.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if len(initial.Transfers) != 1 {
		t.Fatalf("created transfers=%d", len(initial.Transfers))
	}
	id := initial.Transfers[0].TransferID
	prefixDigest := sha256.Sum256(data[:17])
	response = request(http.MethodPatch, "/"+id+"/content", data[:17], map[string]string{"Content-Type": "application/offset+octet-stream", "Upload-Offset": "0", "Upload-Digest": "sha256=" + hex.EncodeToString(prefixDigest[:])})
	_ = response.Body.Close()
	if offset, e := client.Offset(ctx, id); e != nil || offset != 17 {
		t.Fatalf("committed offset=%d err=%v", offset, e)
	}
	if _, e := service.PublishedPath(ctx, id); e == nil {
		t.Fatal("partial upload was published")
	}
	_ = client.Close()
	reconnect()
	if err = durable.Close(); err != nil {
		t.Fatal(err)
	}
	partial, err := os.OpenFile(filepath.Join(root, "spool", id+".part"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = partial.Write([]byte("uncommitted crash tail")); err != nil {
		t.Fatal(err)
	}
	if err = partial.Close(); err != nil {
		t.Fatal(err)
	}
	openStorage()
	client = newClient("file-token")
	if offset, e := client.Offset(ctx, id); e != nil || offset != 17 {
		t.Fatalf("restart committed offset=%d err=%v", offset, e)
	}
	batch, err := client.SendBatch(ctx, "batch_upload", "session_transfer", []clienttransfer.Source{{Basename: "upload.bin", Size: int64(len(data)), SHA256: sum, Reader: bytes.NewReader(data)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Transfers) != 1 || batch.Transfers[0].TransferID != id {
		t.Fatal("resume changed transfer identity")
	}
	published, err := service.PublishedPath(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(published)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("upload publication integrity err=%v", err)
	}
	if _, err = os.Stat(filepath.Join(root, "spool", id+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("upload partial remains: %v", err)
	}

	// Reverse data is staged through the source endpoint's real local API.
	localToken := strings.Repeat("s", 32)
	localHandler, err := hostserver.NewNativeLocalFileTransferHandler(hostserver.LocalFileTransferConfig{Token: localToken, MachineID: "machine_transfer", Service: service, ResolveRecipient: func(sessionID, destinationID string) (string, error) {
		if sessionID != "session_transfer" || destinationID != "cli_transfer" {
			return "", errors.New("recipient unavailable")
		}
		return "cli_transfer", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	localHTTP := httptest.NewServer(localHandler)
	defer localHTTP.Close()
	sender := &clienttransfer.LocalSender{Endpoint: localHTTP.URL + "/v1/local-file-transfers", Token: localToken, HTTPClient: localHTTP.Client()}
	type sendResult struct {
		batch clienttransfer.Batch
		err   error
	}
	sendCtx, stopSend := context.WithCancel(ctx)
	sendDone := make(chan sendResult, 1)
	go func() {
		batch, e := sender.SendNativeBatch(sendCtx, "batch_reverse", "machine_transfer", "cli_transfer", "account_transfer", "session_transfer", []clienttransfer.Source{{Basename: "reverse.bin", Size: int64(len(data)), SHA256: sum, Reader: bytes.NewReader(data)}}, now.Add(time.Minute))
		sendDone <- sendResult{batch, e}
	}()
	var sent *sendResult
	defer func() {
		stopSend()
		if sent == nil {
			select {
			case result := <-sendDone:
				sent = &result
			case <-time.After(2 * time.Second):
				t.Error("source sender did not stop")
			}
		}
	}()
	pending, err := client.Pending(ctx, "session_transfer", 1)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	reverseID := pending[0].TransferID
	destination := filepath.Join(root, "client-inbox")
	interruptedClient := &transferInterruptedDownload{NativeClient: client, interrupt: reconnect}
	receiver, err := inbox.New(inbox.Config{Client: interruptedClient, MachineID: "cli_transfer", SessionID: "session_transfer", Path: destination})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = receiver.Deliver(ctx, pending[0]); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("interrupted download err=%v", err)
	}
	localPartial := filepath.Join(destination, ".paperboat-transfer-"+reverseID+".part")
	if partial, e := os.Stat(localPartial); e != nil || partial.Size() != 17 {
		t.Fatalf("interrupted download partial=%v err=%v", partial, e)
	}
	if _, err = os.Stat(filepath.Join(destination, "reverse.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial reverse download became visible")
	}
	_ = client.Close()
	reconnect()
	client = newClient("file-token")
	receiver, err = inbox.New(inbox.Config{Client: client, MachineID: "cli_transfer", SessionID: "session_transfer", Path: destination})
	if err != nil {
		t.Fatal(err)
	}
	relative, err := receiver.Deliver(ctx, pending[0])
	if err != nil {
		t.Fatal(err)
	}
	if !rangeSeen.Load() {
		t.Fatal("reverse resume did not request committed prefix offset")
	}
	actual, err = os.ReadFile(filepath.Join(destination, strings.TrimPrefix(relative, "Paperboat Inbox/")))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("reverse publication integrity err=%v", err)
	}
	repeated, err := receiver.Deliver(ctx, pending[0])
	if err != nil || repeated != relative {
		t.Fatalf("duplicate reverse publication path=%q err=%v", repeated, err)
	}
	if err = client.Receipt(ctx, reverseID, "stored", relative); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-sendDone:
		sent = &result
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if sent.err != nil || len(sent.batch.Transfers) != 1 || sent.batch.Transfers[0].TransferID != reverseID {
		t.Fatalf("source receipt batch=%+v err=%v", sent.batch, sent.err)
	}
	if current, e := service.Get(ctx, reverseID); e != nil || current.State != "delivered" {
		t.Fatalf("receipt state=%s err=%v", current.State, e)
	}
	if _, err = os.Stat(filepath.Join(root, "spool", reverseID+".content")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source payload remains after receipt: %v", err)
	}
	if _, err = os.Stat(localPartial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reverse partial remains: %v", err)
	}
	denied := newClient("wrong-token")
	if _, err = denied.Offset(ctx, id); err == nil {
		t.Fatal("HTTP credential rejection missing")
	}
	t.Logf("bounded bidirectional transfer: payload_bytes=%d elapsed=%s (includes crash/reconnect; not sustained throughput)", 2*len(data), time.Since(transferStarted))
	if !relayed && directTraffic.packets.Load() == 0 {
		t.Fatal("transfer fixture observed no direct WireGuard data")
	}
	if relayed && relay.Snapshot().Forwarded == 0 {
		t.Fatal("file transfer bypassed forced DERP/QUIC")
	}
}

// Cut the real response after a bounded prefix and kill its native session. The
// production Inbox must retain its own partial and resume it after reconstruction.
type transferInterruptedDownload struct {
	*clienttransfer.NativeClient
	interrupt func()
}

func (c *transferInterruptedDownload) Content(ctx context.Context, manifest clienttransfer.Manifest, offset int64) (*http.Response, error) {
	response, err := c.NativeClient.Content(ctx, manifest, offset)
	if err != nil {
		return nil, err
	}
	response.Body = &transferCutBody{ReadCloser: response.Body, remaining: 17, interrupt: c.interrupt}
	return response, nil
}

type transferCutBody struct {
	io.ReadCloser
	remaining int
	interrupt func()
}

func (b *transferCutBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		b.interrupt()
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	return n, err
}

// Count actual WireGuard transport records, excluding discovery and handshake
// packets, so the direct fixture cannot pass solely through its DERP bootstrap.
type transferDirectTraffic struct{ packets atomic.Uint64 }

func (r *transferDirectTraffic) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	c, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	udp, ok := c.(*net.UDPConn)
	if !ok {
		_ = c.Close()
		return nil, net.ErrClosed
	}
	return &transferDirectPacket{UDPConn: udp, recorder: r}, nil
}

type transferDirectPacket struct {
	*net.UDPConn
	recorder *transferDirectTraffic
}

func (c *transferDirectPacket) count(p []byte, n int, err error) {
	if err == nil && n == len(p) && len(p) >= 32 && binary.LittleEndian.Uint32(p) == 4 {
		c.recorder.packets.Add(1)
	}
}
func (c *transferDirectPacket) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.UDPConn.WriteTo(p, addr)
	c.count(p, n, err)
	return n, err
}
func (c *transferDirectPacket) WriteToUDPAddrPort(p []byte, addr netip.AddrPort) (int, error) {
	n, err := c.UDPConn.WriteToUDPAddrPort(p, addr)
	c.count(p, n, err)
	return n, err
}

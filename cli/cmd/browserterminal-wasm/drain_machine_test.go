//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeattachment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// drainEdge is what the edge (edge/internal/runtime TestBrowserTerminalDrainAcrossUpdates) tells the machine.
type drainEdge struct {
	NodeID      string `json:"node_id"`
	Epoch       string `json:"epoch"`
	H2          string `json:"h2"`
	H3          string `json:"h3"`
	SPKI        string `json:"spki"`
	Chain       string `json:"chain"`
	BrowserURL  string `json:"browser_url"`
	EdgeHost    string `json:"edge_host"`
	RuntimeHost string `json:"runtime_host"`
}

// drainTicket is one browser terminal admission the edge hands out, in order, one per browser connection.
type drainTicket struct {
	Credential        string `json:"credential"`
	TerminalSessionID string `json:"terminal_session_id"`
	AttachmentID      string `json:"attachment_id"`
}

const (
	drainAccount     = "account_drain"
	drainMachineID   = "machine_drain"
	drainEnvironment = "env_drain"
	drainIssuer      = "https://control.drain.example.test"
	drainRoute       = "runtime_route_drain"
	// drainCap stands for runtimewire.DrainLimit: the first generation's drain context ends here, the way the cap
	// ends it, so the browser it still holds moves to the current generation.
	drainCap = 12 * time.Second
)

// TestBrowserTerminalDrainMachine is the machine of the cross-process pb update drain check: one session owner (the
// PTY survives updates) and three worker generations, each with its own runtime attachment, browser terminal
// endpoint and output publisher, plus the browser (a native client from protocol.go and flow.go). It runs only when
// the edge test starts it (PAPERBOAT_DRAIN_DIR).
//
// Timeline: the browser attaches on generation 1; generation 2 takes over (generation 1 drains and keeps the
// browser); generation 3 takes over (generation 2 holds nothing and leaves; generation 1 still keeps the browser);
// generation 1's drain reaches its cap and it shuts down. Through both handovers the browser sees no close and no
// pause; at the cap exactly one close 4301 "moved", one reattach to generation 3, and its output resumes with no
// marker lost or repeated.
func TestBrowserTerminalDrainMachine(t *testing.T) {
	dir := os.Getenv("PAPERBOAT_DRAIN_DIR")
	if dir == "" {
		t.Skip("run by edge/internal/runtime TestBrowserTerminalDrainAcrossUpdates")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var edge drainEdge
	readJSONFile(t, ctx, filepath.Join(dir, "edge.json"), &edge)
	m := newDrainMachine(t, ctx, dir, edge)

	browser := &drainBrowser{t: t, edge: edge, machine: m, events: make(chan string, 64)}
	var err error
	if browser.identity, err = newBrowserIdentity(time.Now()); err != nil {
		t.Fatal(err)
	}
	tickets := make([]drainTicket, 4)
	for i := range tickets {
		tickets[i] = m.ticket(t, i+1, browser.identity.Fingerprint)
	}
	writeJSONFile(t, filepath.Join(dir, "tickets.json"), tickets)

	m.start(t, ctx, 1)
	go browser.run(ctx)
	browser.waitMarkers(t, ctx, 50) // attached on generation 1, output flowing

	m.start(t, ctx, 2)
	capCtx, capReached := context.WithCancel(ctx)
	drained := make(chan error, 1)
	go func() { drained <- m.gens[1].attachment.DrainBrowserTerminals(capCtx) }()
	time.Sleep(4 * time.Second) // the handover (goaway, 2 s quiet) and a drain with the browser held

	m.start(t, ctx, 3)
	if err := m.gens[2].attachment.DrainBrowserTerminals(ctx); err != nil {
		t.Fatalf("generation 2 (holding no terminal) did not finish its drain: %v", err)
	}
	m.stop(t, 2)
	time.Sleep(3 * time.Second)
	select {
	case err := <-drained:
		t.Fatalf("generation 1 ended its drain while it still held the browser: %v", err)
	default:
	}
	before := browser.snapshot()

	capReached()
	if err := <-drained; !errors.Is(err, context.Canceled) {
		t.Fatalf("generation 1's drain ended with %v at its cap", err)
	}
	m.stop(t, 1)
	browser.waitReattached(t, ctx, 1)
	browser.waitMarkers(t, ctx, before.markers+100) // resumed on generation 3
	after := browser.snapshot()
	cancel()

	if before.connections != 1 {
		t.Fatalf("the browser reconnected %d times across two handovers, want never", before.connections-1)
	}
	if before.worstGap > drainGapBound {
		t.Fatalf("the browser's output paused %v across the handovers (worst gap), want under %v", before.worstGap, drainGapBound)
	}
	if after.connections != 2 || len(after.closes) != 1 || after.closes[0] != "4301 moved" {
		t.Fatalf("at the cap the browser saw %d connections and closes %v, want exactly one reattach after 4301 moved", after.connections, after.closes)
	}
	if after.duplicates != 0 || after.missing != 0 {
		t.Fatalf("output across the cap reattach: %d markers repeated, %d lost", after.duplicates, after.missing)
	}
	t.Logf("held through two handovers: %d markers, worst gap %v; one 4301 reattach; %d markers after, none lost or repeated", before.markers, before.worstGap, after.markers-before.markers)
}

// drainGapBound: the ticker writes a marker every 20 ms; a shared CI runner's scheduling, not the product, sets
// how far above that one gap may go. A reattach costs a WebSocket dial, a TLS handshake, an attach and a screen,
// and every reattach is counted separately, exactly.
const drainGapBound = 750 * time.Millisecond

type drainMachine struct {
	root, dir   string
	edge        drainEdge
	sessions    *session.Manager
	launcher    *process.ShellLauncher
	created     session.Snapshot
	machineKey  ed25519.PrivateKey
	rootPublic  ed25519.PublicKey
	terminalID  server.BrowserTerminalIdentity
	signer      ed25519.PrivateKey
	browserAuth server.AuthorizerFactory
	public      string
	thumbprint  string
	gens        map[uint64]*drainGeneration
}

type drainGeneration struct {
	attachment *runtimeattachment.Service
	runtime    *server.Server
	http       *http.Server
}

func newDrainMachine(t *testing.T, ctx context.Context, dir string, edge drainEdge) *drainMachine {
	t.Helper()
	m := &drainMachine{root: t.TempDir(), dir: dir, edge: edge, gens: map[uint64]*drainGeneration{}}
	store, err := identity.Open(identity.Config{StateRoot: m.root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	now := time.Now().UTC()
	if err := store.SaveRegistration(identity.Registration{ServerURL: drainIssuer, MachineID: drainMachineID, EnvironmentID: drainEnvironment, PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(m.root, "inbox"), InstallationGeneration: 1, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMachineControl(identity.MachineControl{MachineID: drainMachineID, EnvironmentID: drainEnvironment, InstallationGeneration: 1, Credential: strings.Repeat("c", 32), ExpiresAt: now.Add(time.Hour), KeyID: key.ID}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(key.Public())
	m.public, m.thumbprint = base64.RawURLEncoding.EncodeToString(key.Public()), "sha256:"+base64.RawURLEncoding.EncodeToString(digest[:])

	// The account root certifies the machine's browser terminal key (the console pins the root).
	rootPublic, rootPrivate := mustKey(t)
	machinePublic, machinePrivate := mustKey(t)
	certificate, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{AccountID: drainAccount, Role: endpointidentity.RoleMachine, EndpointID: drainMachineID, QUICPublicKey: machinePublic, Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := certificate.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := endpointidentity.NewTLSCertificate(certificate, rootPublic, machinePrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m.rootPublic, m.machineKey = rootPublic, machinePrivate
	m.terminalID = server.BrowserTerminalIdentity{Certificate: raw, RootKeyID: accountRootKeyID(rootPublic), TLSCertificate: leaf}

	signerPublic, signer := mustKey(t)
	m.signer = signer
	revocations := auth.NewRevocationCache()
	verifier := auth.Verifier{Keys: drainKeys{signerPublic}, Clock: drainClock{}, Revocations: revocations, ClockSkew: time.Minute}
	m.browserAuth, err = hostruntime.NewBrowserTerminalCredentialAuthorizer(hostruntime.CredentialAuthConfig{Issuer: drainIssuer, EnvironmentID: drainEnvironment, MachineID: drainMachineID, HelperID: "helper_drain", Verifier: verifier, Revocations: revocations})
	if err != nil {
		t.Fatal(err)
	}

	adapter, err := pty.NewAdapter(m.root)
	if err != nil {
		t.Fatal(err)
	}
	m.sessions, err = session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 4, MaxAttachments: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = m.sessions.Shutdown(shutdown)
	})
	shell := ""
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash", "/bin/zsh", "/usr/bin/zsh", "/bin/sh", "/usr/bin/sh"} {
		// The shell launcher takes only a canonical executable, never a symlink.
		if info, err := os.Lstat(candidate); err == nil && info.Mode().IsRegular() {
			shell = candidate
			break
		}
	}
	if shell == "" {
		t.Fatal("no canonical shell executable")
	}
	// A marker every 20 ms: the browser checks each one arrives once, in order, without a pause.
	ticker := `i=0; while :; do printf 'M%d\n' "$i"; i=$((i+1)); sleep 0.02; done`
	m.created, err = m.sessions.Create(ctx, session.CreateRequest{ID: "drain", Name: "drain", Command: pty.Command{Path: shell, Args: []string{"-c", ticker}, CWD: m.root, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	m.launcher, err = process.NewShellLauncher(shell, []string{"PATH=/usr/bin:/bin", "TERM=xterm", "SHELL=" + shell}, m.sessions)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ticket mints the n-th browser credential, bound to the browser's TLS key, as the control plane would.
func (m *drainMachine) ticket(t *testing.T, n int, browserKey string) drainTicket {
	t.Helper()
	now := time.Now().UTC()
	attachment := "att_drain_" + strconv.Itoa(n)
	claims := auth.Claims{Issuer: drainIssuer, Audience: "paperboat-machine", Subject: "user_drain", JTI: "jti_drain_" + strconv.Itoa(n), IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(10 * time.Minute).Unix(), Scope: []string{"terminal:operate"}, CredentialClass: "browser_terminal_operation", EnvironmentID: drainEnvironment, AccountID: drainAccount, UserID: "user_drain", MachineID: drainMachineID, SessionID: m.created.ID, BrowserAttachmentID: attachment, BrowserPublicKeySHA256: browserKey, ExpectedGeneration: int64(m.created.Generation), PolicyGeneration: 1}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "drain-key", "typ": "paperboat-credential+jwt"})
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	credential := input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(m.signer, []byte(input)))
	return drainTicket{Credential: credential, TerminalSessionID: m.created.ID, AttachmentID: attachment}
}

// admission is the server's runtime carrier admission of generation n on the edge.
func (m *drainMachine) admission(n uint64) runtimewire.Admission {
	return runtimewire.Admission{Schema: runtimewire.Schema, RouteKind: runtimewire.RouteKind, Hostname: m.edge.RuntimeHost, RouteRevision: 1, AttachmentGeneration: n,
		ConfigContentHash: "sha256:" + strings.Repeat("0", 64), EdgeEndpoints: []string{m.edge.H2, m.edge.H3}, ExpiresAt: time.Now().Add(time.Hour),
		Binding: runtimewire.Binding{AccountID: drainAccount, HostID: drainMachineID, TunnelID: drainMachineID, ConnectorID: drainMachineID, SessionID: "session_gen_" + strconv.FormatUint(n, 10),
			ProcessGeneration: n, ConfigGeneration: 1, RouteID: drainRoute, RouteGeneration: 1, InstallationGeneration: 1, EdgeNodeID: m.edge.NodeID, EdgeProcessEpoch: m.edge.Epoch,
			EdgeCarrierServerSPKISHA256: m.edge.SPKI, EdgeCarrierServerCertificateChainPEM: m.edge.Chain, MachineIdentityPublicKey: m.public, MachineIdentityThumbprint: m.thumbprint}}
}

// start runs worker generation n: the server names its carrier (the edge pulls admissions.json), then it attaches.
func (m *drainMachine) start(t *testing.T, ctx context.Context, n uint64) {
	t.Helper()
	admission := m.admission(n)
	writeJSONFile(t, filepath.Join(m.dir, "admissions.json"), []runtimewire.Admission{admission})
	time.Sleep(300 * time.Millisecond) // the edge pulls every 50 ms

	broadcast, err := browserbroadcastserver.NewRegistry(m.sessions, func(context.Context) (ed25519.PrivateKey, error) { return m.machineKey, nil })
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{Sessions: m.sessions, BrowserOutput: broadcast, Health: health.New("drain", []string{"terminal.v1", "health.v1"}, nil), SessionLauncher: m.launcher, WorkspaceRoot: m.root, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := operation.NewJournal(64)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer, err := server.New(server.Config{Negotiator: protocol.Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true}}, Journal: journal, Handler: dispatcher, MaxConcurrent: 16, HeartbeatInterval: time.Hour, MutationDeadline: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewBrowserTerminalWebSocketHandler(server.BrowserTerminalWebSocketHandlerConfig{Server: runtimeServer, MaxConnections: 8, Authorizer: m.browserAuth, Identity: func(context.Context) (server.BrowserTerminalIdentity, error) { return m.terminalID, nil }})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/browser-terminal", handler)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()

	answer, err := json.Marshal(map[string]any{"data": struct {
		runtimewire.Admission
		ActiveEdgeNodeID       string `json:"active_edge_node_id"`
		ActiveEdgeProcessEpoch string `json:"active_edge_process_epoch"`
	}{admission, m.edge.NodeID, m.edge.Epoch}})
	if err != nil {
		t.Fatal(err)
	}
	control := drainControl(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != runtimewire.HostPath {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(answer)), Header: http.Header{"Content-Type": {"application/json"}}, Request: r}, nil
	})
	attachment, err := runtimeattachment.New(runtimeattachment.Config{ControlURL: drainIssuer, StateRoot: m.root, Transport: control, MachineID: drainMachineID, WorkerGeneration: n, InstallationGeneration: 1, ListenAddress: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := attachment.BindBroadcaster(broadcast); err != nil {
		t.Fatal(err)
	}
	if err := attachment.Start(ctx); err != nil {
		t.Fatal(err)
	}
	m.gens[n] = &drainGeneration{attachment: attachment, runtime: runtimeServer, http: httpServer}
	t.Cleanup(func() { m.stop(t, n) })
}

// stop is a worker generation exiting: its runtime attachment closes its carriers, its endpoint stops.
func (m *drainMachine) stop(t *testing.T, n uint64) {
	g := m.gens[n]
	if g == nil {
		return
	}
	delete(m.gens, n)
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := g.attachment.Shutdown(shutdown); err != nil {
		t.Errorf("generation %d runtime attachment shutdown: %v", n, err)
	}
	_ = g.http.Close()
	_ = g.runtime.Shutdown(shutdown)
}

// drainBrowser is the console's browser terminal, natively: protocol.go and flow.go over the edge's WebSocket.
type drainBrowser struct {
	t        *testing.T
	edge     drainEdge
	machine  *drainMachine
	identity *browserIdentity
	events   chan string

	mu          sync.Mutex
	connections int
	closes      []string
	seen        map[int]int // marker -> times delivered as output
	shown       map[int]bool
	next        int
	lastAt      time.Time
	worstGap    time.Duration
	markers     int
	duplicates  int
	partial     []byte // delivered output after its last newline: a marker may span two records
}

type drainSnapshot struct {
	connections, markers, duplicates, missing int
	closes                                    []string
	worstGap                                  time.Duration
}

func (b *drainBrowser) snapshot() drainSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	missing := 0
	for i := 0; i < b.next; i++ {
		if b.seen[i] == 0 && !b.shown[i] {
			missing++
		}
	}
	return drainSnapshot{connections: b.connections, markers: b.markers, duplicates: b.duplicates, missing: missing, closes: append([]string(nil), b.closes...), worstGap: b.worstGap}
}

func (b *drainBrowser) waitMarkers(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	for b.snapshot().markers < n {
		select {
		case <-ctx.Done():
			t.Fatalf("the browser saw %d of %d markers", b.snapshot().markers, n)
		case event := <-b.events:
			t.Log(event)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (b *drainBrowser) waitReattached(t *testing.T, ctx context.Context, closes int) {
	t.Helper()
	for {
		s := b.snapshot()
		if len(s.closes) >= closes && s.connections > closes {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the browser did not reattach: closes %v, connections %d", s.closes, s.connections)
		case event := <-b.events:
			t.Log(event)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// run connects, and reconnects at once after the edge closes it, as the console does for a moved close.
func (b *drainBrowser) run(ctx context.Context) {
	b.seen, b.shown = map[int]int{}, map[int]bool{}
	var from uint64
	for ctx.Err() == nil {
		next, closed, err := b.connect(ctx, from)
		from = next
		if closed != "" {
			b.mu.Lock()
			b.closes = append(b.closes, closed)
			b.mu.Unlock()
		}
		if err != nil && ctx.Err() == nil {
			b.event("connection ended: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (b *drainBrowser) event(format string, args ...any) {
	select {
	case b.events <- fmt.Sprintf(format, args...):
	default:
	}
}

// connect runs one browser connection; it returns the output sequence to resume from and the edge's close.
func (b *drainBrowser) connect(ctx context.Context, from uint64) (uint64, string, error) {
	header := http.Header{"Origin": {"https://console.drain.example.test"}}
	ws, _, err := websocket.Dial(ctx, b.edge.BrowserURL, &websocket.DialOptions{Host: b.edge.EdgeHost, HTTPHeader: header, CompressionMode: websocket.CompressionDisabled,
		Subprotocols: []string{websocketSubprotocol, "pb-ticket." + strings.Repeat("A", 43)}})
	if err != nil {
		return from, "", err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(websocketMessageMax)
	kind, first, err := ws.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || len(first) < 2 || first[0] != 0 {
		return from, closeOf(err), fmt.Errorf("identity envelope: %w", err)
	}
	peer, err := verifyIdentityEnvelope(first[1:], b.machine.rootPublic, accountRootKeyID(b.machine.rootPublic), drainAccount, drainMachineID, time.Now())
	if err != nil {
		return from, "", err
	}
	conn := newDrainWSConn(ctx, ws)
	client := tls.Client(conn, tlsClientConfig(b.identity, peer, time.Now))
	if err := client.HandshakeContext(ctx); err != nil {
		return from, conn.closed(), err
	}
	hello, _ := randomID("request")
	if err := writeStructuredFrame(client, protocol.Frame{Type: "hello", RequestID: hello, Version: protocol.ProtocolVersion, Payload: helloPayload("")}); err != nil {
		return from, conn.closed(), err
	}
	if welcome, err := readStructuredFrame(client); err != nil || welcome.Type != "welcome" {
		return from, conn.closed(), fmt.Errorf("welcome: %v", err)
	}
	requestID, _ := randomID("request")
	operationID, _ := randomID("operation")
	payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": b.machine.created.ID, "from_sequence": from})
	if err := writeStructuredFrame(client, protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: operationID, Capability: "terminal.v1", DeadlineMS: 10000, Payload: payload}); err != nil {
		return from, conn.closed(), err
	}
	response, err := readStructuredFrame(client)
	if err != nil || response.Type != "response" {
		return from, conn.closed(), fmt.Errorf("attach: %v %s", err, response.Payload)
	}
	state, err := parseAttachResponse(response.Payload)
	if err != nil {
		return from, conn.closed(), err
	}
	screen, cursor, err := readScreenCheckpoint(client, state.StreamID)
	if err != nil {
		return from, conn.closed(), err
	}
	b.mu.Lock()
	b.connections++
	b.mu.Unlock()
	b.output(screen, true)
	b.event("attached (connection %d) at %d", b.snapshot().connections, cursor)

	keys := make(chan browserbroadcast.Epoch, 4)
	failed := make(chan error, 2)
	go func() { failed <- b.readTLS(client, state.StreamID, keys) }()
	machinePublic := peer.Certificate.Claims.QUICPublicKey
	epochs := map[[16]byte]browserbroadcast.Epoch{}
	indexes := map[[16]byte]uint64{}
	var pending [][]byte
	for {
		select {
		case <-ctx.Done():
			return cursor, "", ctx.Err()
		case err := <-failed:
			return cursor, conn.closed(), err
		case <-conn.done:
			return cursor, conn.closed(), conn.err
		case epoch := <-keys:
			epochs[epoch.ID] = epoch
		case record := <-conn.records:
			pending = append(pending, record)
		}
		for len(pending) > 0 {
			hint, err := browserbroadcast.EpochHint(pending[0])
			if err != nil {
				return cursor, conn.closed(), err
			}
			epoch, ok := epochs[hint]
			if !ok {
				break
			}
			record, err := browserbroadcast.Open(pending[0], epoch, machinePublic)
			if err != nil || record.SessionID != b.machine.created.ID || record.Generation != state.Generation {
				return cursor, conn.closed(), fmt.Errorf("output record: %v", err)
			}
			pending = pending[1:]
			previous := indexes[hint]
			if previous != 0 && record.Index != previous+1 || record.StartSequence > cursor {
				return cursor, conn.closed(), errors.New("output dropped (the browser would resync)")
			}
			indexes[hint] = record.Index
			end := record.StartSequence + uint64(len(record.Data))
			if end <= cursor {
				continue
			}
			b.output(record.Data[cursor-record.StartSequence:], false)
			cursor = end
		}
	}
}

// readTLS reads the inner TLS channel: output keys go to keys; anything else after attach is ignored.
func (b *drainBrowser) readTLS(stream io.Reader, streamID uint32, keys chan<- browserbroadcast.Epoch) error {
	for {
		kind, payload, err := readApplicationFrame(stream)
		if err != nil {
			return err
		}
		if kind != appKindBinary {
			continue
		}
		frame, err := protocol.DecodeTerminalOutput(payload)
		if err != nil || frame.StreamID != streamID {
			return fmt.Errorf("terminal output frame: %v", err)
		}
		if frame.Channel == protocol.TerminalBroadcastKey && len(frame.Data) == 49 && frame.Data[0] == 1 {
			var epoch browserbroadcast.Epoch
			copy(epoch.ID[:], frame.Data[1:17])
			copy(epoch.Key[:], frame.Data[17:])
			keys <- epoch
		}
	}
}

// A marker line in delivered output; on a restored screen a marker is any M<digits> not followed by a digit.
var (
	drainMarker       = regexp.MustCompile(`M(\d+)\r?\n`)
	drainScreenMarker = regexp.MustCompile(`M(\d+)(?:[^\d]|$)`)
)

// output records the markers in delivered output (each must come once, in order) or on a restored screen.
func (b *drainBrowser) output(data []byte, screen bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	pattern := drainScreenMarker
	if !screen {
		data = append(b.partial, data...)
		cut := bytes.LastIndexByte(data, '\n') + 1
		b.partial = append([]byte(nil), data[cut:]...)
		data, pattern = data[:cut], drainMarker
	} else {
		b.partial = nil // a restored screen replaces the half line
	}
	for _, match := range pattern.FindAllSubmatch(data, -1) {
		i, _ := strconv.Atoi(string(match[1]))
		if screen {
			b.shown[i] = true
			if i >= b.next {
				b.next = i + 1
			}
			continue
		}
		b.seen[i]++
		if b.seen[i] > 1 {
			b.duplicates++
		}
		if i >= b.next {
			b.next = i + 1
		}
		b.markers++
		if !b.lastAt.IsZero() && now.Sub(b.lastAt) > b.worstGap && b.connections == 1 {
			b.worstGap = now.Sub(b.lastAt)
		}
		b.lastAt = now
	}
}

// drainWSConn is the browser's TLS transport over the edge WebSocket: 0x00 carries TLS, 0x01 a sealed output record.
type drainWSConn struct {
	ctx     context.Context
	ws      *websocket.Conn
	in      *io.PipeReader
	records chan []byte
	done    chan struct{}
	err     error
	close   string
}

func newDrainWSConn(ctx context.Context, ws *websocket.Conn) *drainWSConn {
	reader, writer := io.Pipe()
	c := &drainWSConn{ctx: ctx, ws: ws, in: reader, records: make(chan []byte, 256), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			kind, message, err := ws.Read(ctx)
			if err == nil && (kind != websocket.MessageBinary || len(message) < 1) {
				err = errors.New("invalid browser terminal message")
			}
			if err != nil {
				c.err, c.close = err, closeOf(err)
				_ = writer.CloseWithError(err)
				return
			}
			switch message[0] {
			case 0:
				if _, err := writer.Write(message[1:]); err != nil {
					return
				}
			case 1:
				c.records <- message[1:]
			default:
				c.err = errors.New("the edge dropped output behind a flood")
				_ = writer.CloseWithError(c.err)
				return
			}
		}
	}()
	return c
}

func (c *drainWSConn) closed() string {
	select {
	case <-c.done:
		return c.close
	case <-time.After(2 * time.Second):
		return ""
	}
}

func (c *drainWSConn) Read(p []byte) (int, error) { return c.in.Read(p) }
func (c *drainWSConn) Write(p []byte) (int, error) {
	if err := c.ws.Write(c.ctx, websocket.MessageBinary, append([]byte{0}, p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *drainWSConn) Close() error                     { return c.ws.CloseNow() }
func (c *drainWSConn) LocalAddr() net.Addr              { return drainAddr{} }
func (c *drainWSConn) RemoteAddr() net.Addr             { return drainAddr{} }
func (c *drainWSConn) SetDeadline(time.Time) error      { return nil }
func (c *drainWSConn) SetReadDeadline(time.Time) error  { return nil }
func (c *drainWSConn) SetWriteDeadline(time.Time) error { return nil }

type drainAddr struct{}

func (drainAddr) Network() string { return "websocket" }
func (drainAddr) String() string  { return "edge" }

// closeOf names a WebSocket close as "<code> <reason>", or "" when the connection dropped without one.
func closeOf(err error) string {
	var closed websocket.CloseError
	if errors.As(err, &closed) {
		return strconv.Itoa(int(closed.Code)) + " " + closed.Reason
	}
	return ""
}

type drainControl func(*http.Request) (*http.Response, error)

func (f drainControl) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type drainKeys struct{ key ed25519.PublicKey }

func (k drainKeys) Lookup(_ context.Context, id string) (ed25519.PublicKey, bool, error) {
	return k.key, id == "drain-key", nil
}
func (drainKeys) Refresh(context.Context) error { return nil }

type drainClock struct{}

func (drainClock) Now() time.Time { return time.Now() }

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func readJSONFile(t *testing.T, ctx context.Context, path string, value any) {
	t.Helper()
	for {
		if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, value) == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never appeared", path)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// writeJSONFile replaces path atomically, so the other process never reads half a file.
func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

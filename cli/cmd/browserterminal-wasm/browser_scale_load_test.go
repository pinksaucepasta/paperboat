//go:build load && (!js || !wasm)

package main

// F21-164: one account holds 100 browser terminals through the real console and edge, the 101st is refused with the
// plan message, closing frees a place. Not part of CI. This is the browser's own wire path (ticket, wss to the edge,
// signed machine identity, TLS inside the socket, attach, sealed output records) driven by the package's protocol code
// without a browser, so 100 of them fit in one process.
//
//	PB_LOAD_ORIGIN=<console origin> PB_LOAD_STATE=<storage state file> PB_LOAD_MACHINE=<machine id> \
//	  [PB_LOAD_TERMINALS=100] [PB_LOAD_RESULT=<json out>] go test -tags load -run TestLoadBrowserTerminalsOverEdge -v -timeout 30m
//
// Nothing secret or terminal-shaped is printed: only counts, timings and the refusal text.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

type loadAPI struct {
	origin string
	cookie string
	csrf   string
	http   *http.Client
}

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Message) }

func newLoadAPI(t *testing.T, origin, statePath string) *loadAPI {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read storage state: %v", err)
	}
	var state struct {
		Cookies []struct{ Name, Value, Domain string } `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("storage state: %v", err)
	}
	host := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	var cookies []string
	for _, c := range state.Cookies {
		if c.Domain == host || strings.TrimPrefix(c.Domain, ".") == host {
			cookies = append(cookies, c.Name+"="+c.Value)
		}
	}
	if len(cookies) == 0 {
		t.Fatalf("storage state has no cookie for %s", host)
	}
	a := &loadAPI{origin: origin, cookie: strings.Join(cookies, "; "), http: &http.Client{Timeout: 60 * time.Second}}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if err := a.call("GET", "/api/v1/auth/csrf", nil, &out); err != nil || out.CSRF == "" {
		t.Fatalf("csrf: %v", err)
	}
	a.csrf = out.CSRF
	return a
}

// call sends one console API request and decodes the {data} envelope into out.
func (a *loadAPI) call(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, a.origin+path, reader)
	req.Header.Set("Cookie", a.cookie)
	req.Header.Set("Origin", a.origin)
	req.Header.Set("Accept", "application/json")
	if method != "GET" {
		req.Header.Set("X-CSRF-Token", a.csrf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error struct{ Code, Message string }
		// the proxy's own refusals are not wrapped in {"error"}
		Code, Message string
	}
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode >= 300 {
		if env.Error.Code == "" {
			env.Error.Code, env.Error.Message = env.Code, env.Message
		}
		return &apiError{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	if out != nil {
		if len(env.Data) == 0 {
			env.Data = raw
		}
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

type loadTicket struct {
	StreamURL       string   `json:"stream_url"`
	Subprotocols    []string `json:"subprotocols"`
	Role            string   `json:"role"`
	MachineID       string   `json:"machine_id"`
	OwnerAccountID  string   `json:"owner_account_id"`
	RootKeyID       string   `json:"root_key_id"`
	RootPublicKey   string   `json:"root_public_key"`
	RootFingerprint string   `json:"root_fingerprint"`
}

// wsConn is the browser's WebSocket as a net.Conn: TLS records ride in messages whose first byte is 0, sealed output
// records are messages that start with 1 (kept apart), 2 says output was dropped behind a flood.
type wsConn struct {
	ws        *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	tlsIn     chan []byte
	records   chan []byte
	buf       []byte
	closeErr  atomic.Value
	writeLock sync.Mutex
}

func newWSConn(ws *websocket.Conn) *wsConn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &wsConn{ws: ws, ctx: ctx, cancel: cancel, tlsIn: make(chan []byte, 256), records: make(chan []byte, 4096)}
	ws.SetReadLimit(websocketMessageMax + 1)
	go c.pump()
	return c
}

func (c *wsConn) pump() {
	defer close(c.tlsIn)
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.closeErr.Store(err)
			return
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case 0:
			if len(data) > 1 {
				c.tlsIn <- data[1:]
			}
		case 1:
			select {
			case c.records <- data[1:]:
			default: // the real client resyncs; this one only counts the hole
			}
		}
	}
}

func (c *wsConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		msg, ok := <-c.tlsIn
		if !ok {
			if err, _ := c.closeErr.Load().(error); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		c.buf = msg
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.writeLock.Lock()
	defer c.writeLock.Unlock()
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, append([]byte{0}, p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *wsConn) Close() error                     { c.cancel(); return c.ws.CloseNow() }
func (c *wsConn) LocalAddr() net.Addr              { return browserAddr("load-local") }
func (c *wsConn) RemoteAddr() net.Addr             { return browserAddr("load-edge") }
func (c *wsConn) SetDeadline(time.Time) error      { return nil }
func (c *wsConn) SetReadDeadline(time.Time) error  { return nil }
func (c *wsConn) SetWriteDeadline(time.Time) error { return nil }

type browserAddr string

func (a browserAddr) Network() string { return "websocket" }
func (a browserAddr) String() string  { return string(a) }

// loadTerm is one attached browser terminal.
type loadTerm struct {
	session   string
	conn      *wsConn
	tls       *tls.Conn
	streamID  uint32
	sequence  atomic.Uint64
	writeLock sync.Mutex
	mu        sync.Mutex
	output    []byte
	notify    chan struct{}
	dead      atomic.Value
	keyMu     sync.Mutex
	keys      map[[16]byte]browserbroadcast.Epoch // the machine's epoch keys, sent on the TLS channel
}

func (lt *loadTerm) appendOutput(b []byte) {
	lt.mu.Lock()
	lt.output = append(lt.output, b...)
	if len(lt.output) > 1<<16 {
		lt.output = lt.output[len(lt.output)-1<<15:]
	}
	lt.mu.Unlock()
	select {
	case lt.notify <- struct{}{}:
	default:
	}
}

func (lt *loadTerm) waitFor(marker string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		lt.mu.Lock()
		found := bytes.Contains(lt.output, []byte(marker))
		lt.mu.Unlock()
		if found {
			return true
		}
		select {
		case <-lt.notify:
		case <-time.After(250 * time.Millisecond):
		case <-deadline:
			return false
		}
	}
}

func (lt *loadTerm) input(data []byte) error {
	lt.writeLock.Lock()
	defer lt.writeLock.Unlock()
	seq := lt.sequence.Add(1)
	frame, err := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: lt.streamID, Sequence: seq, Data: data}, nil)
	if err != nil {
		return err
	}
	return writeApplicationFrame(lt.tls, appKindBinary, frame)
}

// openTerm runs the browser's attach: ticket, wss, identity, TLS, hello, attach, checkpoint.
func openTerm(api *loadAPI, machine, session string, timings map[string]time.Duration) (*loadTerm, error) {
	t0 := time.Now()
	id, err := newBrowserIdentity(time.Now())
	if err != nil {
		return nil, err
	}
	var tk loadTicket
	if err := api.call("POST", "/api/v1/terminal-sessions/"+session+"/browser-ticket", map[string]string{"browser_public_key_sha256": id.Fingerprint}, &tk); err != nil {
		return nil, fmt.Errorf("ticket: %w", err)
	}
	timings["ticket"] = time.Since(t0)
	t1 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, tk.StreamURL, &websocket.DialOptions{Subprotocols: tk.Subprotocols, HTTPHeader: http.Header{"Origin": {api.origin}}})
	if err != nil {
		return nil, fmt.Errorf("dial edge: %w", err)
	}
	conn := newWSConn(ws)
	fail := func(step string, err error) (*loadTerm, error) {
		conn.Close()
		return nil, fmt.Errorf("%s: %w", step, err)
	}
	var envelope []byte
	select {
	case envelope = <-conn.tlsIn:
		if envelope == nil { // the edge closed the socket before the machine's identity: its close code says why
			err, _ := conn.closeErr.Load().(error)
			return fail("edge closed before the machine identity", fmt.Errorf("close status %d: %w", websocket.CloseStatus(err), err))
		}
	case <-ctx.Done():
		return fail("machine identity", ctx.Err())
	}
	rootBytes, _ := base64.RawURLEncoding.DecodeString(tk.RootPublicKey)
	peer, err := verifyIdentityEnvelope(envelope, ed25519.PublicKey(rootBytes), tk.RootKeyID, tk.OwnerAccountID, tk.MachineID, time.Now())
	if err != nil {
		// What arrived instead is the edge's or host's own message (a refusal), never terminal content.
		return fail("machine identity", fmt.Errorf("%w (first message: %d bytes, %.120q)", err, len(envelope), envelope))
	}
	client := tls.Client(conn, tlsClientConfig(id, peer, time.Now))
	if err := client.HandshakeContext(ctx); err != nil {
		return fail("tls", err)
	}
	timings["connect"] = time.Since(t1)
	t2 := time.Now()
	hello := protocol.Frame{Type: "hello", RequestID: "request_" + uuid.NewString(), Version: protocol.ProtocolVersion, Payload: helloPayload(connectionModeTerminal)}
	if err := writeStructuredFrame(client, hello); err != nil {
		return fail("hello", err)
	}
	welcome, err := readStructuredFrame(client)
	if err != nil || welcome.Type != "welcome" {
		return fail("welcome", fmt.Errorf("%v type=%q", err, welcome.Type))
	}
	requestID := "request_" + uuid.NewString()
	payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": session, "from_sequence": 0, "at_live_boundary": false})
	attach := protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: "operation_" + uuid.NewString(), Capability: "terminal.v1", DeadlineMS: 10000, Payload: payload}
	if err := writeStructuredFrame(client, attach); err != nil {
		return fail("attach request", err)
	}
	response, err := readStructuredFrame(client)
	if err != nil || response.Type != "response" || response.RequestID != requestID {
		return fail("attach response", fmt.Errorf("%v type=%q %s", err, response.Type, response.Payload))
	}
	state, err := parseAttachResponse(response.Payload)
	if err != nil {
		return fail("attach response", err)
	}
	if _, _, err := readScreenCheckpoint(client, state.StreamID); err != nil {
		return fail("checkpoint", err)
	}
	timings["attach"] = time.Since(t2)
	lt := &loadTerm{session: session, conn: conn, tls: client, streamID: state.StreamID, notify: make(chan struct{}, 1), keys: map[[16]byte]browserbroadcast.Epoch{}}
	lt.sequence.Store(state.InputSequence)
	go lt.readStream(client)
	go lt.readRecords(conn, peer.Certificate.Claims.QUICPublicKey, session, state.Generation)
	return lt, nil
}

// readStream reads the TLS channel: the machine sends each epoch key here before any sealed record.
func (lt *loadTerm) readStream(client *tls.Conn) {
	for {
		kind, payload, err := readApplicationFrame(client)
		if err != nil {
			lt.dead.Store(err)
			return
		}
		if kind != appKindBinary {
			continue
		}
		frame, err := protocol.DecodeTerminalOutput(payload)
		if err != nil {
			continue
		}
		switch frame.Channel {
		case protocol.TerminalBroadcastKey:
			if len(frame.Data) == 49 && frame.Data[0] == 1 {
				var e browserbroadcast.Epoch
				copy(e.ID[:], frame.Data[1:17])
				copy(e.Key[:], frame.Data[17:])
				lt.keyMu.Lock()
				lt.keys[e.ID] = e
				lt.keyMu.Unlock()
			}
		case protocol.TerminalStdout, protocol.TerminalStderr:
			lt.appendOutput(frame.Data)
		}
	}
}

func (lt *loadTerm) readRecords(conn *wsConn, machine ed25519.PublicKey, session string, generation uint64) {
	for raw := range conn.records {
		hint, err := browserbroadcast.EpochHint(raw)
		if err != nil {
			continue
		}
		var epoch browserbroadcast.Epoch
		var ok bool
		for i := 0; i < 50 && !ok; i++ { // the key arrives on the TLS channel just before the first record
			lt.keyMu.Lock()
			epoch, ok = lt.keys[hint]
			lt.keyMu.Unlock()
			if !ok {
				time.Sleep(20 * time.Millisecond)
			}
		}
		if !ok {
			continue
		}
		record, err := browserbroadcast.Open(raw, epoch, machine)
		if err != nil || record.SessionID != session || record.Generation != generation {
			continue
		}
		lt.appendOutput(record.Data)
	}
}

func (lt *loadTerm) close() {
	lt.conn.Close()
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	return values[min(len(values)-1, int(float64(len(values))*p))]
}

func TestLoadBrowserTerminalsOverEdge(t *testing.T) {
	origin, state, machine := os.Getenv("PB_LOAD_ORIGIN"), os.Getenv("PB_LOAD_STATE"), os.Getenv("PB_LOAD_MACHINE")
	if origin == "" || state == "" || machine == "" {
		t.Skip("set PB_LOAD_ORIGIN, PB_LOAD_STATE and PB_LOAD_MACHINE")
	}
	count := 100
	if v, err := strconv.Atoi(os.Getenv("PB_LOAD_TERMINALS")); err == nil && v > 0 {
		count = v
	}
	api := newLoadAPI(t, origin, state)
	created := struct {
		sync.Mutex
		ids []string
	}{}
	newSession := func() (string, error) {
		var out struct {
			ID string `json:"id"`
		}
		if err := api.call("POST", "/api/v1/machines/"+machine+"/terminal-sessions?workspace=personal", map[string]any{}, &out); err != nil {
			return "", err
		}
		created.Lock()
		created.ids = append(created.ids, out.ID)
		created.Unlock()
		return out.ID, nil
	}
	closeSession := func(id string) error {
		return api.call("POST", "/api/v1/machines/"+machine+"/terminal-sessions/"+id+"/close", nil, nil)
	}
	terms := make([]*loadTerm, count)
	defer func() {
		for _, lt := range terms {
			if lt != nil {
				lt.close()
			}
		}
		created.Lock()
		defer created.Unlock()
		var wg sync.WaitGroup
		gate := make(chan struct{}, 8)
		var failed atomic.Int32
		for _, id := range created.ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				gate <- struct{}{}
				defer func() { <-gate }()
				if err := closeSession(id); err != nil {
					failed.Add(1)
				}
			}()
		}
		wg.Wait()
		t.Logf("cleanup: %d sessions closed, %d failed", len(created.ids), failed.Load())
	}()

	// A session made while the account still has room: its ticket is what the full account refuses.
	spare, err := newSession()
	if err != nil {
		t.Fatalf("create spare session: %v", err)
	}
	result := map[string]any{"terminals": count}
	// 1. open: at most 8 in flight (the server admits 8 tickets per browser login at a time).
	type opened struct {
		ok                                     bool
		err                                    string
		total, create, ticket, connect, attach float64
	}
	results := make([]opened, count)
	gate := make(chan struct{}, 8)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range terms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			t0 := time.Now()
			id, err := newSession()
			if err != nil {
				results[i].err = "create: " + err.Error()
				return
			}
			create := time.Since(t0)
			timings := map[string]time.Duration{}
			var lt *loadTerm
			for try := 0; try < 3; try++ { // one retry class only: a busy refusal
				lt, err = openTerm(api, machine, id, timings)
				var ae *apiError
				if err == nil || !errors.As(err, &ae) || ae.Status != 429 && ae.Status != 503 {
					break
				}
				time.Sleep(time.Duration(1+try) * time.Second)
			}
			if err != nil {
				results[i].err = err.Error()
				return
			}
			terms[i] = lt
			results[i] = opened{ok: true, total: time.Since(t0).Seconds(), create: create.Seconds(), ticket: timings["ticket"].Seconds(), connect: timings["connect"].Seconds(), attach: timings["attach"].Seconds()}
		}()
	}
	wg.Wait()
	var totals, creates, attaches []float64
	errorsSeen := map[string]int{}
	for _, r := range results {
		if r.ok {
			totals, creates, attaches = append(totals, r.total), append(creates, r.create), append(attaches, r.attach)
		} else {
			e := r.err
			if len(e) > 160 {
				e = e[:160]
			}
			errorsSeen[e]++
		}
	}
	result["open_seconds"] = time.Since(start).Seconds()
	result["attached"] = len(totals)
	result["open_p50_s"], result["open_p95_s"], result["open_max_s"] = percentile(totals, .5), percentile(totals, .95), percentile(totals, 1)
	result["create_p95_s"], result["attach_p95_s"] = percentile(creates, .95), percentile(attaches, .95)
	result["open_errors"] = errorsSeen
	t.Logf("attached %d of %d in %.1fs (open p50 %.1f p95 %.1f)", len(totals), count, time.Since(start).Seconds(), percentile(totals, .5), percentile(totals, .95))

	// 2. echo: every attached terminal answers a fresh marker at the same time, three rounds.
	var echoes []float64
	echoFail := 0
	for round := 0; round < 3; round++ {
		var mu sync.Mutex
		var rw sync.WaitGroup
		for i, lt := range terms {
			if lt == nil {
				continue
			}
			rw.Add(1)
			go func() {
				defer rw.Done()
				mark := fmt.Sprintf("PBL%dR%d", i, round)
				// the typed echo has the marker split by the printf format, the output has it whole
				cmd := fmt.Sprintf("printf '%%s_%%s\\n' %s %s\n", mark[:3], mark[3:])
				t0 := time.Now()
				ok := lt.input([]byte(cmd)) == nil && lt.waitFor(mark[:3]+"_"+mark[3:], 60*time.Second)
				mu.Lock()
				defer mu.Unlock()
				if ok {
					echoes = append(echoes, time.Since(t0).Seconds())
				} else {
					echoFail++
				}
			}()
		}
		rw.Wait()
	}
	result["echoes_ok"], result["echoes_failed"] = len(echoes), echoFail
	result["echo_p50_s"], result["echo_p95_s"], result["echo_max_s"] = percentile(echoes, .5), percentile(echoes, .95), percentile(echoes, 1)
	t.Logf("echo %d ok, %d failed, p95 %.1fs", len(echoes), echoFail, percentile(echoes, .95))

	// 3. the (count+1)th is refused with the plan message, both when the session is created and when an
	// existing session asks for its ticket (no socket opens in either case).
	var refusal, createRefusal apiError
	var ae *apiError
	if _, err := newSession(); errors.As(err, &ae) {
		createRefusal = *ae
	}
	_, err = openTerm(api, machine, spare, map[string]time.Duration{})
	if errors.As(err, &ae) {
		refusal = *ae
	}
	result["refusal_status"], result["refusal_code"], result["refusal_message"] = refusal.Status, refusal.Code, refusal.Message
	result["create_refusal_status"], result["create_refusal_code"] = createRefusal.Status, createRefusal.Code
	t.Logf("terminal %d: ticket %v; create %v", count+1, err, &createRefusal)

	// 4. closing frees a place: close one attached terminal, the spare session attaches.
	if terms[0] != nil {
		terms[0].close()
		terms[0] = nil
		time.Sleep(3 * time.Second)
		lt, err := openTerm(api, machine, spare, map[string]time.Duration{})
		result["opens_after_close"] = err == nil
		if err != nil {
			result["opens_after_close_error"] = err.Error()
		} else {
			terms[0] = lt
		}
	}
	if out := os.Getenv("PB_LOAD_RESULT"); out != "" {
		raw, _ := json.MarshalIndent(result, "", "  ")
		_ = os.WriteFile(out, raw, 0o600)
	}
	if len(totals) != count || echoFail != 0 || refusal.Code != "browser_terminal_limit_reached" || createRefusal.Code != "browser_terminal_limit_reached" {
		t.Fatalf("not clean: attached %d/%d, echo failures %d, refusal %q", len(totals), count, echoFail, refusal.Code)
	}
	if opened, _ := result["opens_after_close"].(bool); !opened {
		t.Fatalf("a closed terminal did not free its place: %v", result["opens_after_close_error"])
	}
}

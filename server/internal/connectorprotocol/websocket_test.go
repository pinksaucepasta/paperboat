package connectorprotocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

type websocketFailureStore struct {
	*persistenceStoreStub
	authErr error
}

func (s *websocketFailureStore) AuthenticateConnector(ctx context.Context, request AuthRequest) (AuthResult, error) {
	if s.authErr != nil {
		return AuthResult{}, s.authErr
	}
	return s.persistenceStoreStub.AuthenticateConnector(ctx, request)
}

func TestWebSocketHandlerRunsCanonicalControlSession(t *testing.T) {
	now := time.Date(2026, 8, 31, 9, 30, 0, 0, time.UTC)
	auth, private := testAuth(t, now, 1)
	snapshot, err := NewSnapshot(auth.TunnelID, 1, testConfigPayload(1, "app.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	store := &persistenceStoreStub{authResult: AuthResult{
		AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID,
		HostID: auth.HostID, IdentityKeyID: auth.IdentityKeyID, IdentityKeyThumbprint: auth.IdentityKeyThumbprint,
		ProcessGeneration: 1, CredentialGeneration: auth.CredentialGeneration,
		CredentialExpiresAt: now.Add(time.Hour), LeaseExpiresAt: now.Add(time.Hour),
	}, snapshot: snapshot}
	persistent, err := NewPersistentServer(store, IdentityProofVerifierFuncs{
		AuthFunc: func(_ context.Context, _ AuthRequest, payload, signature []byte) error {
			if !ed25519.Verify(private.Public().(ed25519.PublicKey), payload, signature) {
				return ErrAuthenticationFailed
			}
			return nil
		},
		RenewalFunc: func(context.Context, RenewalRequest, []byte, []byte) error { return nil },
	}, ServerConfig{
		Capabilities: requiredCapabilityList(), Clock: &testClock{now: now},
		LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second,
		SessionIDs: func() (string, error) { return "session_websocket_01", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewControlTransport(persistent, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewWebSocketHandler(transport)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /v1/tunnels/{tunnel_id}/connectors/{connector_id}/control", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/tunnels/" + auth.TunnelID + "/connectors/" + auth.ConnectorID + "/control"
	connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{Subprotocols: []string{ControlWebSocketSubprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
	defer stream.Close()

	hello := Hello{
		Protocol: ProtocolName, MinVersion: ProtocolVersion, MaxVersion: ProtocolVersion,
		AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID,
		HostID: auth.HostID, ProcessGeneration: 1, Capabilities: requiredCapabilityList(), Auth: auth,
	}
	frame, err := NewFrame(MessageHello, "hello_websocket_01", hello)
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteFrame(stream, frame); err != nil {
		t.Fatal(err)
	}
	welcomeFrame, err := ReadFrame(stream)
	if err != nil || welcomeFrame.Type != MessageWelcome {
		t.Fatalf("welcome frame=%+v err=%v", welcomeFrame, err)
	}
	var welcome Welcome
	if err = welcomeFrame.DecodePayload(&welcome); err != nil {
		t.Fatal(err)
	}
	snapshotFrame, err := ReadFrame(stream)
	if err != nil || snapshotFrame.Type != MessageSnapshot {
		t.Fatalf("snapshot frame=%+v err=%v", snapshotFrame, err)
	}
	disconnect := Disconnect{
		AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID,
		SessionID: welcome.SessionID, ProcessGeneration: 1, Reason: ReasonProtocolClosed,
	}
	frame, err = NewFrame(MessageDisconnect, "disconnect_websocket_01", disconnect)
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteFrame(stream, frame); err != nil {
		t.Fatal(err)
	}
	// Wait for the HTTP handler and its deferred durable disconnect write to
	// finish before inspecting the intentionally simple persistence test stub.
	// httptest.Server.Close blocks until active handlers have returned.
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if len(store.created) != 1 || len(store.disconnect) != 1 {
		t.Fatalf("created=%d disconnected=%d", len(store.created), len(store.disconnect))
	}
}

func TestWebSocketHandlerRejectsQueryCredentialsBeforeUpgrade(t *testing.T) {
	handler := &WebSocketHandler{transport: &ControlTransport{}}
	request := httptest.NewRequest(http.MethodGet, "/v1/tunnels/tunnel_01/connectors/connector_01/control?token=secret", nil)
	request.SetPathValue("tunnel_id", "tunnel_01")
	request.SetPathValue("connector_id", "connector_01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestWebSocketHandlerReportsUnexpectedAuthenticationFailureOnce(t *testing.T) {
	const privateCause = "private-key=never-log-this"
	cases := []struct {
		name          string
		storeErr      error
		minVersion    string
		maxVersion    string
		malformed     bool
		truncated     bool
		invalidType   bool
		wantLevel     string
		wantMessage   string
		wantCode      string
		wantOutcome   string
		wantCause     string
		wantReference bool
	}{
		{
			name:      "pure credential rejection",
			storeErr:  ErrAuthenticationFailed,
			wantLevel: "WARN", wantMessage: "connector control stream rejected",
			wantCode: string(CodeAuthenticationFailed), wantOutcome: "rejected",
		},
		{
			name:      "pure revoked connector rejection",
			storeErr:  ErrConnectorNotFound,
			wantLevel: "WARN", wantMessage: "connector control stream rejected",
			wantCode: string(CodeAuthenticationFailed), wantOutcome: "rejected",
		},
		{
			name:      "pure cancellation",
			storeErr:  context.Canceled,
			wantLevel: "INFO", wantMessage: "connector control stream canceled",
			wantCode: string(CodeAuthenticationFailed), wantOutcome: "canceled",
		},
		{
			name:       "pure version rejection",
			minVersion: "2.0", maxVersion: "2.0",
			wantLevel: "WARN", wantMessage: "connector control stream rejected",
			wantCode: string(CodeProtocolIncompatible), wantOutcome: "rejected",
		},
		{
			name:        "pure malformed client frame",
			malformed:   true,
			wantLevel:   "WARN",
			wantMessage: "connector control stream rejected",
			wantCode:    string(CodeMalformedFrame),
			wantOutcome: "rejected",
		},
		{
			name: "client frame field type rejection", invalidType: true,
			wantLevel: "WARN", wantMessage: "connector control stream rejected",
			wantCode: string(CodeMalformedFrame), wantOutcome: "rejected",
		},
		{
			name:        "truncated client frame",
			truncated:   true,
			wantLevel:   "WARN",
			wantMessage: "connector control stream rejected",
			wantCode:    string(CodeMalformedFrame),
			wantOutcome: "rejected",
		},
		{
			name:      "truncated provider response",
			storeErr:  io.ErrUnexpectedEOF,
			wantLevel: "ERROR", wantMessage: "operation failed",
			wantCode: string(CodeAuthenticationFailed), wantOutcome: "failed",
			wantReference: true,
		},
		{
			name:      "joined provider failure and credential rejection",
			storeErr:  errors.Join(context.Canceled, syscall.ECONNREFUSED, errors.New(privateCause), ErrAuthenticationFailed),
			wantLevel: "ERROR", wantMessage: "operation failed",
			wantCode: string(CodeAuthenticationFailed), wantOutcome: "failed",
			wantCause: "connection_refused", wantReference: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			previous := slog.Default()
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			now := time.Now().UTC()
			auth, private := testAuth(t, now, 1)
			snapshot, err := NewSnapshot(auth.TunnelID, 1, testConfigPayload(1, "app.example.test"))
			if err != nil {
				t.Fatal(err)
			}
			store := &websocketFailureStore{persistenceStoreStub: &persistenceStoreStub{
				authResult: AuthResult{
					AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID,
					HostID: auth.HostID, IdentityKeyID: auth.IdentityKeyID, IdentityKeyThumbprint: auth.IdentityKeyThumbprint,
					ProcessGeneration: 1, CredentialGeneration: auth.CredentialGeneration,
					CredentialExpiresAt: now.Add(time.Hour), LeaseExpiresAt: now.Add(time.Hour),
				},
				snapshot: snapshot,
			}, authErr: test.storeErr}
			persistent, err := NewPersistentServer(store, IdentityProofVerifierFuncs{
				AuthFunc: func(_ context.Context, _ AuthRequest, payload, signature []byte) error {
					if !ed25519.Verify(private.Public().(ed25519.PublicKey), payload, signature) {
						return ErrAuthenticationFailed
					}
					return nil
				},
				RenewalFunc: func(context.Context, RenewalRequest, []byte, []byte) error { return nil },
			}, ServerConfig{
				Capabilities: requiredCapabilityList(), Clock: &testClock{now: now},
				LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second,
				SessionIDs: func() (string, error) { return "session_websocket_01", nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			transport, err := NewControlTransport(persistent, nil)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewWebSocketHandler(transport)
			if err != nil {
				t.Fatal(err)
			}
			reference := supportreport.NewReference()
			handlerDone := make(chan struct{})
			mux := http.NewServeMux()
			mux.Handle("GET /v1/tunnels/{tunnel_id}/connectors/{connector_id}/control", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				ctx := serverdiagnostics.WithCorrelationID(r.Context(), "request_connector_ws_01")
				ctx = supportreport.WithFailureIdentity(ctx, "", "request_connector_ws_01", reference)
				handler.ServeHTTP(w, r.WithContext(ctx))
			}))
			server := httptest.NewServer(mux)
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/tunnels/" + auth.TunnelID + "/connectors/" + auth.ConnectorID + "/control"
			connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{Subprotocols: []string{ControlWebSocketSubprotocol}})
			if err != nil {
				t.Fatal(err)
			}
			defer connection.CloseNow()
			hello := Hello{
				Protocol: ProtocolName, MinVersion: ProtocolVersion, MaxVersion: ProtocolVersion,
				AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID,
				HostID: auth.HostID, ProcessGeneration: 1, Capabilities: requiredCapabilityList(), Auth: auth,
			}
			if test.minVersion != "" {
				hello.MinVersion, hello.MaxVersion = test.minVersion, test.maxVersion
			}
			var frame Frame
			if test.malformed {
				payload, marshalErr := json.Marshal(hello)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				payload = append(payload[:len(payload)-1], []byte(`,"unexpected":"`+privateCause+`"}`)...)
				frame = Frame{Type: MessageHello, Version: ProtocolVersion, RequestID: "hello_websocket_failure_01", Payload: payload}
			} else if test.minVersion == "" {
				frame, err = NewFrame(MessageHello, "hello_websocket_failure_01", hello)
			} else {
				var payload []byte
				payload, err = json.Marshal(hello)
				frame = Frame{Type: MessageHello, Version: ProtocolVersion, RequestID: "hello_websocket_failure_01", Payload: payload}
			}
			if err != nil {
				t.Fatal(err)
			}
			stream := websocket.NetConn(ctx, connection, websocket.MessageBinary)
			if test.invalidType {
				body := []byte(`{"type":"hello","version":17,"request_id":"private-key=never-log-this","payload":{}}`)
				var prefix [4]byte
				binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
				err = writeAll(stream, append(prefix[:], body...))
			} else if test.truncated {
				err = writeAll(stream, []byte{0, 0, 0, 3, 'x'})
				if err == nil {
					_ = connection.Close(websocket.StatusNormalClosure, "")
				}
			} else if test.minVersion == "" && !test.malformed {
				err = WriteFrame(stream, frame)
			} else {
				err = writeFrameUnchecked(stream, frame)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := connection.Read(ctx); err == nil {
				t.Fatal("failed control session unexpectedly remained open")
			}
			_ = connection.CloseNow()
			select {
			case <-handlerDone:
			case <-ctx.Done():
				t.Fatal("websocket handler did not finish after peer close")
			}
			server.Close()

			var event map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &event); err != nil {
				t.Fatalf("expected exactly one structured event: %v; logs=%q", err, logs.String())
			}
			for key, want := range map[string]string{
				"level": test.wantLevel, "msg": test.wantMessage,
				"code": test.wantCode, "outcome": test.wantOutcome,
			} {
				if got, _ := event[key].(string); got != want {
					t.Errorf("%s=%q, want %q", key, got, want)
				}
			}
			if test.wantCause != "" {
				if got, _ := event["cause"].(string); got != test.wantCause {
					t.Errorf("cause=%q, want %q", got, test.wantCause)
				}
				if errno, _ := event["errno"].(float64); errno <= 0 {
					t.Errorf("errno=%v, want preserved system errno", event["errno"])
				}
				if got, _ := event["stage"].(string); got != "http_handler" {
					t.Errorf("stage=%q, want http_handler", got)
				}
				if got, _ := event["correlation_id"].(string); got != "request_connector_ws_01" {
					t.Errorf("correlation_id=%q", got)
				}
			}
			if test.wantReference {
				if got, _ := event["support_reference"].(string); got != reference {
					t.Errorf("support_reference=%q, want generated request reference", got)
				}
			}
			if strings.Contains(logs.String(), privateCause) {
				t.Fatal("private provider error text was logged")
			}
		})
	}
}

func writeFrameUnchecked(writer io.Writer, frame Frame) error {
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	if err := writeAll(writer, prefix[:]); err != nil {
		return err
	}
	return writeAll(writer, body)
}

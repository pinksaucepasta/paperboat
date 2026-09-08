//go:build (darwin || linux) && paperboat_native_e2e

package codexsession

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	hostserver "github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

type nativeCodexAuthorizer struct{ sessionID string }

func (a nativeCodexAuthorizer) Authorize(context.Context, protocol.Frame) (hostserver.Authorization, error) {
	return hostserver.Authorization{SessionID: a.sessionID, ResourceID: a.sessionID}, nil
}

// TestNativeCodexAppServerReconnect launches the supported Codex binary, then
// proves a fresh WebSocket can initialize the same live app-server after the
// first client disconnects. It is opt-in because Codex is not a Go test tool.
func TestNativeCodexAppServerReconnect(t *testing.T) {
	codexPath := os.Getenv("PAPERBOAT_NATIVE_E2E_CODEX_PATH")
	if !filepath.IsAbs(codexPath) {
		t.Fatal("PAPERBOAT_NATIVE_E2E_CODEX_PATH must name an absolute Codex binary")
	}
	workspace := t.TempDir()
	manager, err := New(Config{StateRoot: t.TempDir(), WorkspaceRoot: workspace, Environment: os.Environ(), CodexPath: codexPath})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "cdx_native_reconnect"
	if _, err := manager.Prepare(t.Context(), sessionID, workspace, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.Stop(ctx, sessionID)
	})
	handler, err := NewHandler(HandlerConfig{Manager: manager, Authorizer: func(token string) (hostserver.Authorizer, error) {
		if token != "codex-connect-token" {
			return nil, errors.New("credential rejected")
		}
		return nativeCodexAuthorizer{sessionID: sessionID}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/codex-sessions/{session_id}/ws", handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/codex-sessions/" + sessionID + "/ws"

	for attempt := 1; attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		socket, _, dialErr := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer codex-connect-token"}}, CompressionMode: websocket.CompressionDisabled})
		if dialErr != nil {
			cancel()
			t.Fatalf("connect %d: %v", attempt, dialErr)
		}
		request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": attempt, "method": "initialize", "params": map[string]any{}})
		if err := socket.Write(ctx, websocket.MessageText, request); err != nil {
			_ = socket.CloseNow()
			cancel()
			t.Fatal(err)
		}
		messageType, response, readErr := socket.Read(ctx)
		var envelope struct {
			ID int `json:"id"`
		}
		decodeErr := json.Unmarshal(response, &envelope)
		if readErr != nil || messageType != websocket.MessageText || decodeErr != nil || envelope.ID != attempt {
			_ = socket.CloseNow()
			cancel()
			t.Fatalf("initialize %d type=%d response=%q error=%v", attempt, messageType, response, readErr)
		}
		_ = socket.Close(websocket.StatusNormalClosure, "reconnect")
		cancel()
	}
}

package controlplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
)

type scopedNodeCredential struct{}

func (scopedNodeCredential) RuntimeNode(_ context.Context, credential string) (string, error) {
	if credential == "node-secret" {
		return "node_a", nil
	}
	return "", nodeauth.ErrNotFound
}
func (scopedNodeCredential) RuntimeCredential(context.Context, string) (string, error) {
	return "node-secret", nil
}
func (scopedNodeCredential) IsSelfHosted(context.Context, string) (bool, error) { return true, nil }

func TestScopedNodeControlAuthorization(t *testing.T) {
	s := &EdgeService{credential: "managed-secret", nodeCredentials: scopedNodeCredential{}}
	for _, tc := range []struct {
		name, path, body string
		want             bool
	}{
		{"own heartbeat", "/v1/nodes/heartbeat", `{"edge_node_id":"node_a"}`, true},
		{"other heartbeat", "/v1/nodes/heartbeat", `{"edge_node_id":"node_b"}`, false},
		{"own relay", "/v1/relay/nodes/start", `{"node_id":"node_a"}`, true},
		{"other relay", "/v1/relay/nodes/start", `{"node_id":"node_b"}`, false},
		{"conflicting identity", "/v1/relay/nodes/start", `{"node_id":"node_a","edge_node_id":"node_b"}`, false},
		{"duplicate identity", "/v1/nodes/register", `{"edge_node_id":"node_b","edge_node_id":"node_a"}`, false},
		{"unbound assignment", "/v1/edge/assignments/current", `{"environment_id":"other"}`, false},
		{"oversized", "/v1/nodes/register", strings.Repeat(" ", maxEdgeDocument+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer node-secret")
			got, err := s.authorizeNodeRequest(r)
			if err != nil || got != tc.want {
				t.Fatalf("authorized=%v err=%v want %v", got, err, tc.want)
			}
		})
	}
	for _, path := range []string{"/v1/edge/browser-terminal/control"} {
		for _, tc := range []struct {
			name, method, node, epoch string
			want                      bool
		}{
			{"own", "GET", "node_a", "epoch_a", true},
			{"foreign", "GET", "node_b", "epoch_a", false},
			{"missing node", "GET", "", "epoch_a", false},
			{"missing epoch", "GET", "node_a", "", false},
			{"wrong method", "POST", "node_a", "epoch_a", false},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				r := httptest.NewRequest(tc.method, path, nil)
				r.Header.Set("Authorization", "Bearer node-secret")
				r.Header.Set("X-Paperboat-Edge-Node-ID", tc.node)
				r.Header.Set("X-Paperboat-Edge-Process-Epoch", tc.epoch)
				allowed, err := s.authorizeNodeRequest(r)
				if err != nil || allowed != tc.want {
					t.Fatalf("authorized=%t err=%v want=%t", allowed, err, tc.want)
				}
			})
		}
	}
	r := httptest.NewRequest("POST", "/v1/nodes/heartbeat", strings.NewReader(`{"edge_node_id":"node_a"}`))
	r.Header.Add("Authorization", "Bearer node-secret")
	r.Header.Add("Authorization", "Bearer managed-secret")
	if ok, err := s.authorizeNodeRequest(r); err != nil || ok {
		t.Fatal("ambiguous authorization accepted")
	}
}

type nodeCredentialError struct{ err error }

type requestBodyFailure struct {
	value    []byte
	readErr  error
	closeErr error
}

func (b *requestBodyFailure) Read(p []byte) (int, error) {
	if len(b.value) == 0 {
		return 0, b.readErr
	}
	n := copy(p, b.value)
	b.value = b.value[n:]
	return n, b.readErr
}

func (b *requestBodyFailure) Close() error { return b.closeErr }

func (r nodeCredentialError) RuntimeNode(context.Context, string) (string, error) {
	return "", r.err
}
func (nodeCredentialError) RuntimeCredential(context.Context, string) (string, error) {
	return "", nodeauth.ErrNotFound
}
func (nodeCredentialError) IsSelfHosted(context.Context, string) (bool, error) { return false, nil }

func TestScopedNodeAuthorizationPreservesRequestReadAndCloseFailures(t *testing.T) {
	service := &EdgeService{credential: "managed-secret", nodeCredentials: scopedNodeCredential{}}
	request := httptest.NewRequest("POST", "/v1/nodes/heartbeat", nil)
	request.Header.Set("Authorization", "Bearer node-secret")
	request.Body = &requestBodyFailure{
		value:    []byte(`{"edge_node_id":"node_a"`),
		readErr:  syscall.EIO,
		closeErr: syscall.ECONNRESET,
	}
	allowed, err := service.authorizeNodeRequest(request)
	if allowed || !errors.Is(err, syscall.EIO) || !errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) {
		t.Fatalf("body failure was lost or accepted: allowed=%t err=%v", allowed, err)
	}
}

func TestRefusedNodeRequestNamesItsCaller(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(previous)
	r := httptest.NewRequest("POST", "/v1/relay/nodes/start", strings.NewReader(`{"node_id":"node_b"}`))
	r.RemoteAddr, r.Header = "203.0.113.9:4444", http.Header{"X-Forwarded-For": {"198.51.100.7"}}
	s := &EdgeService{credential: "managed-secret", nodeCredentials: scopedNodeCredential{}}
	if ok, err := s.authorizeNodeRequest(r); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	warnNodeRefusal(r)
	for _, want := range []string{"203.0.113.9:4444", "198.51.100.7", "claimed_node_id=node_b", "/v1/relay/nodes/start"} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("log lacks %q: %s", want, logged.String())
		}
	}
}

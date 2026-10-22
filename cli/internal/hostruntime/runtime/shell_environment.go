//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
)

// ShellEnvironmentPath serves managed ENV to `pb env hook` shells started
// outside Paperboat (plain ssh, VS Code Remote-SSH). It lives on the
// loopback worker service behind the owner-only local control token, so only
// the unix user that owns this runtime can call it.
const ShellEnvironmentPath = "/v1/shell-environment"

// shellEnvironmentTTL bounds how long a delivered set is reused without the
// control plane. Values stay in this process's memory only.
const shellEnvironmentTTL = 60 * time.Second

// shellEnvironmentFailureTTL keeps a failed delivery (no network, pending)
// answering at once, so only the first shell after it waits for the timeout.
const shellEnvironmentFailureTTL = 30 * time.Second

const shellEnvironmentTimeout = 5 * time.Second

// ownerShellEnvironment is the runtime's ENV source for its own account. The
// actor is always the installation's recipient account: a local caller can
// never read another account's values, even one that may launch here.
type ownerShellEnvironment interface {
	OwnerShellEnvironment(ctx context.Context, workspace, account string) ([]string, error)
}

type shellEnvironmentHandler struct {
	source ownerShellEnvironment
	token  string
	now    func() time.Time
	mu     sync.Mutex
	cached map[string]*shellEnvironmentEntry
}

// shellEnvironmentEntry is one fetch: done closes when values/err are set.
type shellEnvironmentEntry struct {
	done    chan struct{}
	values  []string
	err     error
	expires time.Time
}

type shellEnvironmentRequest struct {
	WorkspaceID string `json:"workspace_id"`
	AccountID   string `json:"account_id"`
}

func newShellEnvironmentHandler(source ownerShellEnvironment, token string) http.Handler {
	if source == nil || strings.TrimSpace(token) == "" {
		return nil
	}
	return &shellEnvironmentHandler{source: source, token: token, now: time.Now, cached: map[string]*shellEnvironmentEntry{}}
}

func (h *shellEnvironmentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.token)) != 1 {
		writeShellEnvironmentError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		writeShellEnvironmentError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	in, err := decodeShellEnvironmentRequest(r.Body)
	if err != nil {
		writeShellEnvironmentError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	values, err := h.environment(r.Context(), in)
	switch {
	case errors.Is(err, errShellEnvironmentAccount):
		writeShellEnvironmentError(w, http.StatusForbidden, "account_mismatch")
	case err != nil:
		writeShellEnvironmentError(w, http.StatusServiceUnavailable, envinject.LaunchNoticeFor(err))
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"values": values}})
	}
}

var errShellEnvironmentAccount = errors.New("shell environment account is not this runtime's account")

func (h *shellEnvironmentHandler) environment(ctx context.Context, in shellEnvironmentRequest) ([]string, error) {
	entry := h.entry(in)
	select {
	case <-entry.done:
		return entry.values, entry.err
	case <-ctx.Done():
		return nil, &envinject.DeliveryError{Cause: ctx.Err()}
	}
}

// entry returns the cached or in-flight fetch for one request, starting a
// fetch that outlives a caller who gives up so its answer is cached.
func (h *shellEnvironmentHandler) entry(in shellEnvironmentRequest) *shellEnvironmentEntry {
	key := in.WorkspaceID + "\x00" + in.AccountID
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry := h.cached[key]; entry != nil {
		select {
		case <-entry.done:
			if h.now().Before(entry.expires) {
				return entry
			}
		default:
			return entry
		}
	}
	entry := &shellEnvironmentEntry{done: make(chan struct{})}
	h.cached[key] = entry
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), shellEnvironmentTimeout)
		defer cancel()
		values, err := h.source.OwnerShellEnvironment(ctx, in.WorkspaceID, in.AccountID)
		h.mu.Lock()
		entry.values, entry.err = values, err
		entry.expires = h.now().Add(shellEnvironmentTTL)
		if err != nil {
			entry.expires = h.now().Add(shellEnvironmentFailureTTL)
		}
		h.mu.Unlock()
		close(entry.done)
	}()
	return entry
}

func decodeShellEnvironmentRequest(body io.Reader) (shellEnvironmentRequest, error) {
	var in shellEnvironmentRequest
	raw, err := io.ReadAll(io.LimitReader(body, 1025))
	if err != nil || len(raw) > 1024 {
		return in, envinject.ErrInvalidSnapshot
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || decoder.More() {
		return in, envinject.ErrInvalidSnapshot
	}
	if _, ok := envinject.LaunchContextFrom(envinject.WithLaunchContext(context.Background(), in.WorkspaceID, in.AccountID)); !ok {
		return in, envinject.ErrInvalidSnapshot
	}
	return in, nil
}

func writeShellEnvironmentError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
}

// OwnerShellEnvironment resolves the installation account's own ENV for one
// workspace, exactly as a launch by that account would receive it.
func (s *layerEnvironmentService) OwnerShellEnvironment(ctx context.Context, workspace, account string) ([]string, error) {
	if s == nil || s.isClosed() {
		return nil, envinject.ErrNotReady
	}
	if err := s.registerLayerRecipient(ctx); err != nil {
		return nil, &envinject.DeliveryError{Cause: err}
	}
	s.mu.RLock()
	owner := s.layerRecipient.RecipientAccount
	s.mu.RUnlock()
	if owner == "" || owner != account {
		return nil, errShellEnvironmentAccount
	}
	return s.layerEnvironmentForLaunch(envinject.WithLaunchContext(ctx, workspace, owner))
}

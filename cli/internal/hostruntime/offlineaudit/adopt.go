package offlineaudit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// AdoptInterval is how long the host waits before it asks again to register a session Paperboat could not take yet.
const AdoptInterval = 30 * time.Second

// Adopter registers the terminal sessions this host opened while Paperboat was unreachable (id terminal_offline_*) with
// POST /v1/machines/{id}/offline-terminal-sessions, as the host's own signed request: the host owns its sessions, so
// none stays unlisted because the client that opened it left early. Queue is Queue.Adopt; an entry goes once the
// server registered the session or can never take it.
type Adopter struct {
	Queue     *Queue
	ServerURL string
	MachineID string
	Client    *http.Client
	Tokens    interface {
		Token(context.Context) (string, error)
	}
	Proofs interface {
		Proof(context.Context, string, string, string, []byte) ([]byte, error)
	}
	OperationID func() (string, error)
	// Interval spaces the retries (zero: AdoptInterval).
	Interval time.Duration
	// Wake ends the wait: the control plane is reachable again (nil never wakes).
	Wake <-chan struct{}
}

// Run registers sessions as they are queued and retries every Interval while the server cannot take one.
func (a *Adopter) Run(ctx context.Context) {
	interval := a.Interval
	if interval <= 0 {
		interval = AdoptInterval
	}
	for {
		var retry <-chan time.Time
		if a.Pass(ctx) {
			retry = time.After(interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-a.Queue.Changed():
		case <-a.Wake:
		case <-retry:
		}
	}
}

// Pass registers the queued sessions, oldest first; it reports whether one is left for a later try.
func (a *Adopter) Pass(ctx context.Context) (retry bool) {
	for a.Queue.Len() > 0 {
		event := a.Queue.Oldest(1)[0]
		permanent, err := a.send(ctx, event)
		if err != nil && !permanent {
			if ctx.Err() == nil {
				diagnosticlog.TryWarn("could not register an offline terminal session with Paperboat; the next attempt follows", "error", err, "queued", a.Queue.Len())
			}
			return true
		}
		if err != nil {
			diagnosticlog.TryWarn("an offline terminal session cannot be registered with Paperboat and was dropped", "error", err)
		}
		if removeErr := a.Queue.Remove(1); removeErr != nil {
			diagnosticlog.TryWarn("could not remove a registered offline terminal session from the queue", "error", removeErr)
			return true
		}
	}
	return false
}

// send posts one session. permanent says the server answered that it will never take it (or the session is gone).
func (a *Adopter) send(ctx context.Context, event Event) (permanent bool, err error) {
	body, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
		AccountID string `json:"account_id"`
	}{event.OperationID, event.PeerAccountID})
	if err != nil {
		return true, err
	}
	path := uploadPathBase + url.PathEscape(a.MachineID) + "/offline-terminal-sessions"
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return true, err
	}
	token, err := a.Tokens.Token(ctx)
	if err != nil {
		return false, fmt.Errorf("register offline terminal session: runtime credential: %w", err)
	}
	operationID, err := a.OperationID()
	if err != nil {
		return false, err
	}
	proof, err := a.Proofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return false, fmt.Errorf("register offline terminal session: machine proof: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	response, err := a.Client.Do(request)
	if err != nil {
		return false, fmt.Errorf("register offline terminal session: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
	switch {
	case response.StatusCode == http.StatusOK:
		return false, nil
	case response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusNotFound:
		return true, fmt.Errorf("register offline terminal session: the server answered %d", response.StatusCode)
	default: // 409 machine offline, 5xx, 429, 403 while the identity renews: try again
		return false, fmt.Errorf("register offline terminal session: the server answered %d", response.StatusCode)
	}
}

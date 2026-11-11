package offlineaudit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

const (
	// MaxBatch is the server's per-request bound.
	MaxBatch       = 500
	uploadTimeout  = 15 * time.Second
	firstRetry     = 5 * time.Second
	maximumRetry   = 5 * time.Minute
	responseLimit  = 64 << 10
	uploadPathBase = "/v1/machines/"
)

// Uploader sends the backlog to POST /v1/machines/{id}/audit-backlog with the machine's runtime
// credential and a body-wide machine proof, oldest first, and removes what the server stored.
type Uploader struct {
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
	// MinInterval spaces the uploads: while sessions run, the backlog goes up at most once per
	// interval and nothing is sent when idle. Zero sends as soon as an event is queued.
	MinInterval time.Duration
	// Wake, when it fires, ends any retry wait: the control plane is reachable again (nil never wakes).
	Wake <-chan struct{}
}

// UploadInterval is the host's cadence for the audit backlog (F5-12).
const UploadInterval = 30 * time.Second

// An upload that never opened a connection to the server (DNS, route or connect failed) costs the server nothing, and
// the backlog of sessions started while Paperboat was unreachable must go up within seconds of the network coming
// back, not after the 5 min backoff of a failing server (measured: 5 min late). Those failures retry from
// unreachableRetry doubling up to unreachableMax (a machine offline for hours settles at the cap), and Wake ends the wait
// at once. Variables so a test can shorten them.
var (
	unreachableRetry = 2 * time.Second
	unreachableMax   = 30 * time.Second
	// retryTimer starts a retry wait; a test replaces it to record the waits instead of sleeping them.
	retryTimer = time.After
)

// neverReachedServer reports whether err happened before any connection to the server existed.
func neverReachedServer(err error) bool {
	var dns *net.DNSError
	var network *net.OpError
	return errors.As(err, &dns) || errors.As(err, &network) && network.Op == "dial"
}

// Run uploads whenever events are queued. A failure the server answered backs off doubling up to maximumRetry; one that
// never reached it retries on the short unreachable schedule.
func (u *Uploader) Run(ctx context.Context) {
	retry, unreachable := firstRetry, 0
	var wait <-chan time.Time
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.Queue.Changed():
		case <-u.Wake:
			unreachable = 0
		case <-wait:
		}
		wait = nil
		if pause := u.MinInterval - time.Since(last); !last.IsZero() && pause > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pause):
			}
		}
		err := u.Flush(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			diagnosticlog.TryWarn("could not upload the offline session audit events; the next attempt follows", "error", err, "queued", u.Queue.Len())
			if neverReachedServer(err) { // MinInterval spaces uploads that reached the server only
				unreachable++
				wait = retryTimer(backoff.Retry(unreachable, unreachableRetry, unreachableMax))
				retry = firstRetry
				continue
			}
			unreachable, last = 0, time.Now()
			wait = time.After(backoff.Jitter(retry))
			retry = min(retry*2, maximumRetry)
			continue
		}
		unreachable, last, retry = 0, time.Now(), firstRetry
	}
}

// Flush uploads the events queued when it starts, one batch at a time. Events recorded meanwhile wait
// for the next flush, so Run's MinInterval holds while sessions keep recording.
func (u *Uploader) Flush(ctx context.Context) error {
	u.dropUnacceptable()
	for pending := u.Queue.Len(); pending > 0; {
		batch := u.Queue.Oldest(min(pending, MaxBatch))
		if len(batch) == 0 {
			return nil
		}
		if err := u.send(ctx, batch); err != nil {
			return err
		}
		if err := u.Queue.Remove(len(batch)); err != nil {
			return err
		}
		pending -= len(batch)
	}
	return nil
}

// dropUnacceptable removes events the server can no longer accept (older than MaxAge), so one
// stale event cannot block the rest; it says how many went.
func (u *Uploader) dropUnacceptable() {
	stale := 0
	for _, event := range u.Queue.Oldest(MaxEvents) {
		if u.Queue.now().Sub(event.OccurredAt) <= MaxAge {
			break
		}
		stale++
	}
	if stale == 0 {
		return
	}
	if err := u.Queue.Remove(stale); err == nil {
		diagnosticlog.TryWarn("offline session audit events older than 7 days could not be delivered and were removed", "count", stale)
	}
}

func (u *Uploader) send(ctx context.Context, batch []Event) error {
	body, err := json.Marshal(struct {
		Events []Event `json:"events"`
	}{batch})
	if err != nil {
		return err
	}
	path := uploadPathBase + url.PathEscape(u.MachineID) + "/audit-backlog"
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token, err := u.Tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("offline audit upload: runtime credential: %w", err)
	}
	operationID, err := u.OperationID()
	if err != nil {
		return err
	}
	proof, err := u.Proofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return fmt.Errorf("offline audit upload: machine proof: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	response, err := u.Client.Do(request)
	if err != nil {
		return fmt.Errorf("offline audit upload: %w", err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, responseLimit))
	if err != nil {
		return fmt.Errorf("offline audit upload: read the answer: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("offline audit upload: the server answered %d", response.StatusCode)
	}
	var result struct {
		Accepted   int `json:"accepted"`
		Duplicates int `json:"duplicates"`
	}
	if err := json.Unmarshal(answer, &result); err != nil || result.Accepted < 0 || result.Duplicates < 0 || result.Accepted+result.Duplicates != len(batch) {
		return fmt.Errorf("offline audit upload: the answer does not account for %d events", len(batch))
	}
	return nil
}

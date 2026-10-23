// Package carriercontrol is the account-private lifecycle boundary of the
// independent native carrier. It carries no application payloads or credentials.
package carriercontrol

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

const schema = "paperboat.carrier-control/v1"
const path = "/v1/carrier/control"

var ErrInvalid = errors.New("invalid native carrier control")
var ErrUnavailable = errors.New("native carrier control unavailable")
var ErrBusy = errors.New("native carrier has active SSH connections")
var ErrFenced = errors.New("native carrier identity or SSH target is fenced")

type request struct {
	Schema       string `json:"schema"`
	SHA256       string `json:"executable_sha256"`
	Operation    string `json:"operation"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
	WorkerID     string `json:"worker_id,omitempty"`
	WorkerEpoch  uint64 `json:"worker_epoch,omitempty"`
}

type response struct {
	Schema string `json:"schema"`
	SHA256 string `json:"executable_sha256"`
	Error  string `json:"error,omitempty"`
	// SSHStreams answers only open-ssh, so clients that never ask never see it.
	SSHStreams *int `json:"ssh_streams,omitempty"`
}

func validHash(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == value
}

func (r request) valid() bool {
	if r.Schema != schema || !validHash(r.SHA256) {
		return false
	}
	if r.Operation == "disable-ssh" || r.Operation == "enable-ssh" {
		return r.SourceSHA256 == "" && validWorkerID(r.WorkerID) && r.WorkerEpoch != 0
	}
	if r.WorkerID != "" || r.WorkerEpoch != 0 {
		return false
	}
	switch r.Operation {
	case "health", "retire-ssh", "resume-ssh", "open-ssh":
		return r.SourceSHA256 == ""
	case "retire-source":
		return validHash(r.SourceSHA256)
	}
	return false
}

func validWorkerID(value string) bool {
	if len(value) == 0 || len(value) > 128 || !asciiAlphaNumeric(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		b := value[i]
		if !asciiAlphaNumeric(b) && b != '.' && b != '_' && b != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func decode(reader io.Reader, value any) error {
	d := json.NewDecoder(reader)
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(value) != nil || d.Decode(&extra) != io.EOF {
		return ErrInvalid
	}
	return nil
}

type Server struct {
	Endpoint string
	Token    []byte
	SHA256   string
	SSH      *managedssh.Host
	// WorkerFence admits only the named active worker epoch and reports when
	// that lease is lost. The release callback remains held through the SSH
	// policy mutation and its cancellation watcher.
	WorkerFence   func(context.Context, string, uint64) (release func(), lost <-chan struct{}, err error)
	RefreshTarget func(context.Context) error
	Ready         chan error
}

func (s *Server) handler() http.Handler {
	capacity := make(chan struct{}, 8)
	workerOperations := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path || r.URL.RawQuery != "" {
			http.Error(w, "invalid carrier operation", http.StatusBadRequest)
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Carrier-Capability"))
		if err != nil || subtle.ConstantTimeCompare(raw, s.Token) != 1 {
			http.Error(w, "carrier access denied", http.StatusUnauthorized)
			return
		}
		select {
		case capacity <- struct{}{}:
			defer func() { <-capacity }()
		default:
			http.Error(w, "carrier control busy", http.StatusTooManyRequests)
			return
		}
		var req request
		if decode(http.MaxBytesReader(w, r.Body, 4096), &req) != nil || !req.valid() {
			http.Error(w, "invalid carrier operation", http.StatusBadRequest)
			return
		}
		result := response{Schema: schema, SHA256: s.SHA256}
		if req.SHA256 != s.SHA256 {
			result.Error = "fenced"
		} else {
			var operationErr error
			switch req.Operation {
			case "open-ssh":
				open := s.SSH.OpenStreams()
				result.SSHStreams = &open
			case "retire-ssh":
				operationErr = s.SSH.Retire()
			case "resume-ssh":
				operationErr = s.SSH.ResumeAdmissions()
			case "retire-source":
				if s.RefreshTarget != nil {
					// A disabled or starting worker may have no usable target.
					// The carrier still knows which source has accepted flows;
					// current/unknown sources remain fenced below.
					_ = s.RefreshTarget(r.Context())
				}
				operationErr = s.SSH.CheckSourceRetirement(req.SourceSHA256)
			case "disable-ssh", "enable-ssh":
				select {
				case workerOperations <- struct{}{}:
					defer func() { <-workerOperations }()
				case <-r.Context().Done():
					operationErr = r.Context().Err()
				}
				if operationErr == nil {
					operationErr = s.workerSSHOperation(r.Context(), req)
				}
			}
			switch {
			case errors.Is(operationErr, managedssh.ErrSSHHostBusy):
				result.Error = "busy"
			case errors.Is(operationErr, managedssh.ErrSSHHostStale), errors.Is(operationErr, hostdproto.ErrFenced):
				result.Error = "fenced"
			case operationErr != nil:
				result.Error = "unavailable"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	})
}

func (s *Server) workerSSHOperation(ctx context.Context, req request) (resultErr error) {
	if s == nil || ctx == nil || s.SSH == nil || s.WorkerFence == nil {
		return hostdproto.ErrFenced
	}
	release, lost, err := s.WorkerFence(ctx, req.WorkerID, req.WorkerEpoch)
	if err != nil {
		if release != nil {
			release()
		}
		return err
	}
	if release == nil || lost == nil {
		if release != nil {
			release()
		}
		return hostdproto.ErrFenced
	}
	defer release()

	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var leaseLost atomic.Bool
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-lost:
			leaseLost.Store(true)
			cancel()
		case <-operationCtx.Done():
		}
	}()
	defer func() {
		cancel()
		<-watcherDone
		select {
		case <-lost:
			leaseLost.Store(true)
		default:
		}
		if leaseLost.Load() {
			resultErr = errors.Join(resultErr, hostdproto.ErrFenced)
		}
	}()

	select {
	case <-lost:
		leaseLost.Store(true)
		cancel()
		return hostdproto.ErrFenced
	case <-operationCtx.Done():
		return operationCtx.Err()
	default:
	}
	if req.Operation == "disable-ssh" {
		return s.SSH.Disable(operationCtx)
	}
	return s.SSH.Enable()
}

func (s *Server) Run(ctx context.Context) (resultErr error) {
	if ctx == nil || s.Endpoint == "" || len(s.Token) != 32 || !validHash(s.SHA256) || s.SSH == nil {
		return ErrInvalid
	}
	listener, err := hostdproto.ListenWorkloads(s.Endpoint)
	if s.Ready != nil {
		select {
		case s.Ready <- err:
		case <-ctx.Done():
			if listener != nil {
				_ = listener.Close()
			}
			return ctx.Err()
		}
	}
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	defer func() {
		closeErr := server.Close()
		removeErr := hostdproto.RemoveWorkloadSocket(s.Endpoint)
		if os.IsNotExist(removeErr) {
			removeErr = nil
		}
		resultErr = errors.Join(resultErr, closeErr, removeErr)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Join(ErrUnavailable, err)
	}
	return nil
}

type Client struct {
	endpoint, sha256 string
	token            []byte
	workerID         string
	workerEpoch      uint64
}

func NewClient(endpoint string, token []byte, sha256 string) (*Client, error) {
	if endpoint == "" || len(token) != 32 || !validHash(sha256) {
		return nil, ErrInvalid
	}
	return &Client{endpoint: endpoint, token: append([]byte(nil), token...), sha256: sha256}, nil
}

// BindWorker returns a client whose explicit SSH capability mutations are
// inseparably tied to one authenticated worker epoch. Lifecycle operations on
// the returned client remain owner-wide and carry no worker identity.
func (c *Client) BindWorker(workerID string, epoch uint64) (*Client, error) {
	if c == nil || c.workerID != "" || c.workerEpoch != 0 || !validWorkerID(workerID) || epoch == 0 {
		return nil, ErrInvalid
	}
	bound := *c
	bound.token = append([]byte(nil), c.token...)
	bound.workerID = workerID
	bound.workerEpoch = epoch
	return &bound, nil
}

func (c *Client) call(ctx context.Context, operation, sourceSHA256 string) error {
	_, err := c.do(ctx, request{Schema: schema, SHA256: c.sha256, Operation: operation, SourceSHA256: sourceSHA256})
	return err
}

func (c *Client) callWorker(ctx context.Context, operation string) error {
	if c == nil || c.workerID == "" || c.workerEpoch == 0 {
		return ErrInvalid
	}
	_, err := c.do(ctx, request{Schema: schema, SHA256: c.sha256, Operation: operation, WorkerID: c.workerID, WorkerEpoch: c.workerEpoch})
	return err
}

func (c *Client) do(ctx context.Context, req request) (response, error) {
	if c == nil || ctx == nil {
		return response{}, ErrInvalid
	}
	if !req.valid() {
		return response{}, ErrInvalid
	}
	body, err := json.Marshal(req)
	if err != nil {
		return response{}, ErrInvalid
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return hostdproto.DialWorkloads(ctx, c.endpoint)
	}, DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second}
	defer transport.CloseIdleConnections()
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, "http://carrier"+path, bytes.NewReader(body))
	if err != nil {
		return response{}, ErrInvalid
	}
	httpReq.Header.Set("X-Paperboat-Carrier-Capability", base64.RawURLEncoding.EncodeToString(c.token))
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalid }}
	httpResponse, err := client.Do(httpReq)
	if err != nil {
		return response{}, errors.Join(ErrUnavailable, err)
	}
	defer httpResponse.Body.Close()
	var result response
	if httpResponse.StatusCode != http.StatusOK || decode(io.LimitReader(httpResponse.Body, 4097), &result) != nil || result.Schema != schema {
		return response{}, ErrUnavailable
	}
	if result.SHA256 != c.sha256 {
		return response{}, ErrFenced
	}
	switch result.Error {
	case "":
		return result, nil
	case "busy":
		return response{}, ErrBusy
	case "fenced":
		return response{}, ErrFenced
	default:
		return response{}, ErrUnavailable
	}
}

func (c *Client) Health(ctx context.Context) error    { return c.call(ctx, "health", "") }
func (c *Client) RetireSSH(ctx context.Context) error { return c.call(ctx, "retire-ssh", "") }
func (c *Client) ResumeSSH(ctx context.Context) error { return c.call(ctx, "resume-ssh", "") }

func (c *Client) DisableSSH(ctx context.Context) error { return c.callWorker(ctx, "disable-ssh") }
func (c *Client) EnableSSH(ctx context.Context) error  { return c.callWorker(ctx, "enable-ssh") }

// OpenSSH is the number of SSH connections the carrier is bridging now. A carrier from before this
// operation answers ErrUnavailable.
func (c *Client) OpenSSH(ctx context.Context) (int, error) {
	result, err := c.do(ctx, request{Schema: schema, SHA256: c.sha256, Operation: "open-ssh"})
	if err != nil {
		return 0, err
	}
	if result.SSHStreams == nil || *result.SSHStreams < 0 {
		return 0, ErrUnavailable
	}
	return *result.SSHStreams, nil
}

func (c *Client) CheckSourceRetirement(ctx context.Context, sourceSHA256 string) error {
	return c.call(ctx, "retire-source", sourceSHA256)
}

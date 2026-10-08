// Package workloadbridge is the private typed session/exec boundary. It never
// owns a PTY, journals input or logs request bodies.
package workloadbridge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 8 << 20

var ErrFenced = errors.New("terminal owner runtime is fenced")

type wireResponse struct {
	Data  json.RawMessage
	Error *wireError
}
type Server struct {
	Endpoint          string
	Token             []byte
	Sessions          session.Service
	Executions        *execprocess.RemoteServer
	Fence             func() hostdproto.Status
	AcquireActive     func(string, uint64) (func(), error)
	Ready             chan error
	MaximumConcurrent int
	mu                sync.Mutex
	worker            string
	epoch             uint64
}

func (s *Server) Run(ctx context.Context) error {
	if len(s.Token) != 32 || s.Sessions == nil || s.Executions == nil || s.Fence == nil || s.MaximumConcurrent < 1 {
		return session.ErrInvalidSession
	}
	listener, err := hostdproto.ListenWorkloads(s.Endpoint)
	if s.Ready != nil {
		s.Ready <- err
	}
	if err != nil {
		return err
	}
	defer hostdproto.RemoveWorkloadSocket(s.Endpoint)
	defer s.Executions.ResetReaders()
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 4096}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	defer close(done)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (s *Server) handler() http.Handler {
	semaphore := make(chan struct{}, s.MaximumConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		token, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Owner-Capability"))
		if err != nil || subtle.ConstantTimeCompare(token, s.Token) != 1 {
			w.WriteHeader(403)
			return
		}
		epoch, err := strconv.ParseUint(r.Header.Get("X-Paperboat-Worker-Epoch"), 10, 64)
		worker := r.Header.Get("X-Paperboat-Worker-ID")
		s.mu.Lock()
		fence := s.Fence()
		if err != nil || epoch == 0 || worker == "" || fence.State != hostdproto.StateActive || fence.WorkerID != worker || fence.Epoch != epoch {
			s.mu.Unlock()
			writeResponse(w, nil, ErrFenced)
			return
		}
		if s.worker != worker || s.epoch != epoch {
			if owner, ok := s.Sessions.(interface{ DetachAllAttachments() error }); ok {
				if err := owner.DetachAllAttachments(); err != nil {
					s.mu.Unlock()
					writeResponse(w, nil, err)
					return
				}
			}
			if err := s.Executions.ResetReaders(); err != nil {
				s.mu.Unlock()
				writeResponse(w, nil, err)
				return
			}
			s.worker, s.epoch = worker, epoch
		}
		s.mu.Unlock()
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		default:
			writeResponse(w, nil, session.ErrResourceLimit)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeResponse(w, nil, session.ErrInvalidSession)
			return
		}
		var result json.RawMessage
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			writeResponse(w, nil, session.ErrInvalidSession)
			return
		}
		if mutates(parts[0], parts[1]) {
			if s.AcquireActive == nil {
				writeResponse(w, nil, ErrFenced)
				return
			}
			release, leaseErr := s.AcquireActive(worker, epoch)
			if leaseErr != nil {
				writeResponse(w, nil, ErrFenced)
				return
			}
			defer release()
		}
		switch parts[0] {
		case "session":
			result, err = session.ServeRemoteCall(ctx, s.Sessions, parts[1], body)
		case "exec":
			result, err = s.Executions.Call(ctx, parts[1], body)
		case "owner":
			if parts[1] != "health" {
				err = session.ErrInvalidSession
			} else {
				result = json.RawMessage(`{}`)
			}
		default:
			err = session.ErrInvalidSession
		}
		writeResponse(w, result, err)
	})
}
func writeResponse(w http.ResponseWriter, data json.RawMessage, err error) {
	var failure *wireError
	if err != nil {
		failure = encodeError(err)
	}
	body, marshalErr := json.Marshal(wireResponse{Data: data, Error: failure})
	if marshalErr != nil || len(body) > maxBody {
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

type Client struct {
	endpoint  string
	token     string
	worker    string
	epoch     uint64
	transport *http.Transport
	http      *http.Client
}

func NewClient(endpoint string, token []byte, worker string, epoch uint64) (*Client, error) {
	if endpoint == "" || len(token) != 32 || worker == "" || epoch == 0 {
		return nil, session.ErrInvalidSession
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return hostdproto.DialWorkloads(ctx, endpoint)
	}, MaxIdleConns: 16, MaxIdleConnsPerHost: 16, IdleConnTimeout: 30 * time.Second}
	return &Client{endpoint: endpoint, token: base64.RawURLEncoding.EncodeToString(token), worker: worker, epoch: epoch, transport: transport, http: &http.Client{Transport: transport}}, nil
}
func (c *Client) Close() { c.transport.CloseIdleConnections() }
func (c *Client) Call(ctx context.Context, kind, op string, body json.RawMessage) (json.RawMessage, error) {
	for {
		data, err := c.callOnce(ctx, kind, op, body)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && ((kind == "session" && op == "WaitNext") || (kind == "exec" && (op == "next" || op == "wait" || op == "reader_next"))) {
			continue
		}
		return data, err
	}
}
func (c *Client) callOnce(ctx context.Context, kind, op string, body json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://terminal-owner/"+kind+"/"+op, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Paperboat-Owner-Capability", c.token)
	req.Header.Set("X-Paperboat-Worker-ID", c.worker)
	req.Header.Set("X-Paperboat-Worker-Epoch", strconv.FormatUint(c.epoch, 10))
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.Join(session.ErrOwnerUnavailable, ctx.Err())
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, session.ErrOwnerUnavailable
	}
	reader := io.LimitReader(response.Body, maxBody+1)
	raw, err := io.ReadAll(reader)
	if err != nil || len(raw) > maxBody {
		return nil, session.ErrOwnerUnavailable
	}
	var out wireResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, session.ErrOwnerUnavailable
	}
	if out.Error != nil {
		return nil, decodeError(out.Error)
	}
	return out.Data, nil
}
func (c *Client) Sessions() (*session.RemoteManager, error) {
	return session.NewRemoteManager(func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		return c.Call(ctx, "session", op, body)
	})
}
func (c *Client) Executions() (*execprocess.RemoteManager, error) {
	return execprocess.NewRemoteManager(func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		return c.Call(ctx, "exec", op, body)
	})
}
func (c *Client) Health(ctx context.Context) error {
	_, err := c.Call(ctx, "owner", "health", json.RawMessage(`{}`))
	return err
}

func mutates(kind, op string) bool {
	if kind == "exec" {
		switch op {
		case "get", "list", "wait", "next", "reader_next":
			return false
		default:
			return true
		}
	}
	if kind == "session" {
		switch op {
		case "ResourceCounts", "Snapshot", "SnapshotAtGeneration", "List", "AttachmentOwnedBy", "Next", "WaitNext", "InputSequence", "QueryInput", "AttachmentStatus":
			return false
		default:
			return true
		}
	}
	return false
}

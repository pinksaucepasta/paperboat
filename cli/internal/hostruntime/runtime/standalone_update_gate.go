//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

var errStandaloneUpdateGate = errors.New("standalone update gate unavailable")

const standaloneUpdateGateSchema = "paperboat.standalone-update-gate/v1"

type standaloneUpdateGateConfig struct {
	MachineID   string
	StatePath   string
	Health      http.Handler
	Workloads   func() hostdproto.WorkloadStatus
	BeginUpdate func(context.Context, string) error
	EndUpdate   func(string) error
	Now         func() time.Time
	// FenceLease bounds how long this process keeps the transfer admission
	// fence for a drained update that is never committed or rolled back.
	// Zero means hostdproto.MaximumUpdateGateDuration.
	FenceLease time.Duration
}

type standaloneUpdateGate struct {
	config          standaloneUpdateGateConfig
	mu              sync.Mutex
	transactions    map[string]standaloneUpdateTransaction
	heldTransaction string
	fenceTimer      *time.Timer
}

type standaloneUpdateTransaction struct {
	Version            string
	Manifest           string
	Path               string
	Status             int
	Samples            uint16
	Target             hostdproto.UpdateGateTargetBinding
	Created            time.Time
	PolicyBound        bool
	Drained            bool
	Committed          bool
	RolledBack         bool `json:",omitempty"`
	WorkloadGeneration uint64
	ProtectedWorkloads uint64
}

type standaloneUpdateGateDisk struct {
	Schema       string                                 `json:"schema"`
	Transactions map[string]standaloneUpdateTransaction `json:"transactions"`
}

func newStandaloneUpdateGate(config standaloneUpdateGateConfig) (*standaloneUpdateGate, error) {
	if config.MachineID == "" || !filepath.IsAbs(config.StatePath) || config.Health == nil || config.Workloads == nil {
		return nil, errStandaloneUpdateGate
	}
	if (config.BeginUpdate == nil) != (config.EndUpdate == nil) {
		return nil, errStandaloneUpdateGate
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.FenceLease <= 0 {
		config.FenceLease = hostdproto.MaximumUpdateGateDuration
	}
	if err := os.MkdirAll(filepath.Dir(config.StatePath), 0o700); err != nil {
		return nil, err
	}
	gate := &standaloneUpdateGate{config: config, transactions: make(map[string]standaloneUpdateTransaction)}
	if err := gate.load(); err != nil {
		return nil, err
	}
	for id, transaction := range gate.transactions {
		if transaction.Drained && !transaction.Committed && config.BeginUpdate != nil {
			if gate.heldTransaction != "" {
				return nil, errStandaloneUpdateGate
			}
			ctx, cancel := context.WithTimeout(context.Background(), hostdproto.MaximumUpdateGateDuration)
			err := config.BeginUpdate(ctx, id)
			cancel()
			if err != nil {
				return nil, err
			}
			gate.holdAdmission(id)
		}
	}
	return gate, nil
}

func (g *standaloneUpdateGate) target() hostdproto.UpdateGateTargetBinding {
	return hostdproto.UpdateGateTargetBinding{Scope: hostdproto.UpdateGateScopeStandalone, MachineID: g.config.MachineID, FailureDomain: "standalone"}
}

func (g *standaloneUpdateGate) HandleUpdateGate(ctx context.Context, request hostdproto.UpdateGateRequest) (hostdproto.UpdateGateResponse, error) {
	if g == nil || ctx == nil || request.Validate() != nil {
		return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.purge()
	target := g.target()
	if request.Operation != hostdproto.UpdateGateTarget && (request.ExpectedTarget == nil || *request.ExpectedTarget != target) {
		return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
	}
	transaction, exists := g.transactions[request.TransactionID]
	switch request.Operation {
	case hostdproto.UpdateGateTarget:
		if exists {
			// Rollback retries resolve their target before presenting the exact
			// completion receipt. Lookup must remain available after rollback;
			// mutating operations enforce their own terminal-state checks below.
			if transaction.Version != request.Version || transaction.Manifest != request.ManifestSHA256 {
				return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
			}
			return hostdproto.UpdateGateResponse{Target: target}, nil
		}
		if len(g.transactions) >= 128 {
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		g.transactions[request.TransactionID] = standaloneUpdateTransaction{Version: request.Version, Manifest: request.ManifestSHA256, Target: target, Created: g.config.Now().UTC()}
	case hostdproto.UpdateGateCandidate:
		if !exists || (transaction.Committed || transaction.RolledBack) || transaction.Version != request.Version || transaction.Manifest != request.ManifestSHA256 || transaction.Target != target || transaction.PolicyBound && (transaction.Path != request.Path || transaction.Status != request.ExpectedStatus || transaction.Samples != request.Samples) {
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		if err := g.probe(ctx, request); err != nil {
			return hostdproto.UpdateGateResponse{}, err
		}
		transaction.Path, transaction.Status, transaction.Samples, transaction.PolicyBound = request.Path, request.ExpectedStatus, request.Samples, true
		g.transactions[request.TransactionID] = transaction
	case hostdproto.UpdateGateDrain:
		if !exists || (transaction.Committed || transaction.RolledBack) || !transaction.PolicyBound || transaction.Version != request.Version || transaction.Manifest != request.ManifestSHA256 || transaction.Target != target {
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		if g.config.BeginUpdate != nil {
			// Updates are serialized by the updater's journal, so a drain for a new
			// transaction means the one still holding the fence was abandoned
			// (its helper died or its rollback never reached this gate).
			if g.heldTransaction != "" && g.heldTransaction != request.TransactionID {
				if err := g.abandonHeld(); err != nil {
					return hostdproto.UpdateGateResponse{}, err
				}
			}
			drainCtx, cancel := context.WithTimeout(ctx, time.Duration(request.TimeoutMillis)*time.Millisecond)
			err := g.config.BeginUpdate(drainCtx, request.TransactionID)
			cancel()
			if err != nil {
				return hostdproto.UpdateGateResponse{}, err
			}
			g.holdAdmission(request.TransactionID)
		}
		workloads := g.config.Workloads()
		if workloads.Generation == 0 && workloads.Protected != 0 {
			return hostdproto.UpdateGateResponse{}, errors.Join(errStandaloneUpdateGate, g.releaseAdmission(request.TransactionID))
		}
		// Executable activation restarts hostd. Terminal owners have independent
		// lifetimes; worker transfer mutations were fenced and drained above.
		transaction.Drained = true
		transaction.WorkloadGeneration = workloads.Generation
		transaction.ProtectedWorkloads = workloads.Protected
		g.transactions[request.TransactionID] = transaction
	case hostdproto.UpdateGateStability:
		if !exists || transaction.RolledBack || !transaction.PolicyBound || (!transaction.Committed && !transaction.Drained) || transaction.Version != request.Version || transaction.Manifest != request.ManifestSHA256 || transaction.Path != request.Path || transaction.Status != request.ExpectedStatus || transaction.Samples != request.Samples || transaction.Target != target {
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		if !transaction.Committed && g.config.BeginUpdate != nil && g.heldTransaction != request.TransactionID {
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		deadline := g.config.Now().Add(time.Duration(request.WindowMillis) * time.Millisecond)
		for {
			probe := request
			probe.TimeoutMillis = request.IntervalMillis
			if err := g.probe(ctx, probe); err != nil {
				return hostdproto.UpdateGateResponse{}, err
			}
			if !g.config.Now().Before(deadline) {
				break
			}
			timer := time.NewTimer(time.Duration(request.IntervalMillis) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return hostdproto.UpdateGateResponse{}, ctx.Err()
			case <-timer.C:
			}
		}
		if transaction.Committed {
			// Recovery may recheck health after metadata commit and before helper
			// cleanup. The exact terminal receipt needs no new fence or write.
			return hostdproto.UpdateGateResponse{Target: target}, nil
		}
	case hostdproto.UpdateGateRollback:
		// Drain may fail before it records Drained=true. The updater still issues
		// rollback to close that exact transaction and re-probe the unchanged
		// active path. Requiring Drained here strands every such recovery forever.
		if !exists || transaction.Committed || !transaction.PolicyBound || transaction.Version != request.Version || transaction.Manifest != request.ManifestSHA256 || transaction.Path != request.Path || transaction.Status != request.ExpectedStatus || transaction.Samples != request.Samples || transaction.Target != target {
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		if err := g.probe(ctx, request); err != nil {
			return hostdproto.UpdateGateResponse{}, err
		}
		transaction.Drained, transaction.RolledBack = false, true
		g.transactions[request.TransactionID] = transaction
	case hostdproto.UpdateGateCommit:
		if exists && transaction.Committed {
			if transaction.Version == request.Version && transaction.Manifest == request.ManifestSHA256 && transaction.Target == target {
				return hostdproto.UpdateGateResponse{Target: target}, g.releaseAdmission(request.TransactionID)
			}
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		if !exists || !transaction.PolicyBound || !transaction.Drained || transaction.Version != request.Version || transaction.Manifest != request.ManifestSHA256 || transaction.Target != target {
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		if g.config.BeginUpdate != nil && g.heldTransaction != request.TransactionID {
			return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
		}
		transaction.Drained, transaction.Committed = false, true
		g.transactions[request.TransactionID] = transaction
	default:
		return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
	}
	if err := g.persist(); err != nil {
		if request.Operation == hostdproto.UpdateGateDrain {
			// The drain is not durable, so the helper will not trust it: do not keep
			// refusing transfers for it.
			transaction.Drained = false
			g.transactions[request.TransactionID] = transaction
			err = errors.Join(err, g.releaseAdmission(request.TransactionID))
		}
		return hostdproto.UpdateGateResponse{}, err
	}
	if request.Operation == hostdproto.UpdateGateCommit || request.Operation == hostdproto.UpdateGateRollback {
		if err := g.releaseAdmission(request.TransactionID); err != nil {
			return hostdproto.UpdateGateResponse{}, err
		}
	}
	return hostdproto.UpdateGateResponse{Target: target}, nil
}

// holdAdmission records that the transfer fence is held for id and arms the
// lease that releases it if the update never finishes.
func (g *standaloneUpdateGate) holdAdmission(id string) {
	if g.fenceTimer != nil {
		g.fenceTimer.Stop()
	}
	g.heldTransaction = id
	g.fenceTimer = time.AfterFunc(g.config.FenceLease, func() { g.expireAdmission(id) })
}

func (g *standaloneUpdateGate) releaseAdmission(id string) error {
	if g.heldTransaction != id {
		return nil
	}
	if err := g.config.EndUpdate(id); err != nil {
		return err
	}
	if g.fenceTimer != nil {
		g.fenceTimer.Stop()
		g.fenceTimer = nil
	}
	g.heldTransaction = ""
	return nil
}

// abandonHeld gives up the fence of a drained update that was neither
// committed nor rolled back and records it as rolled back. A later rollback
// receipt for that transaction is still accepted.
func (g *standaloneUpdateGate) abandonHeld() error {
	id := g.heldTransaction
	if err := g.releaseAdmission(id); err != nil {
		return err
	}
	if transaction, ok := g.transactions[id]; ok && !transaction.Committed {
		transaction.Drained, transaction.RolledBack = false, true
		g.transactions[id] = transaction
	}
	return nil
}

// expireAdmission runs when a drained update has held the fence for the whole
// lease. Without it an update whose helper died after the drain would refuse
// every file transfer on this machine until the next update attempt.
func (g *standaloneUpdateGate) expireAdmission(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.heldTransaction != id || g.abandonHeld() != nil {
		return
	}
	_ = g.persist()
}

func (g *standaloneUpdateGate) probe(ctx context.Context, request hostdproto.UpdateGateRequest) error {
	for range request.Samples {
		recorder := &boundedResponseRecorder{header: make(http.Header), limit: 64 << 10}
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1"+request.Path, nil)
		if err != nil {
			return errStandaloneUpdateGate
		}
		httpRequest.RemoteAddr = "127.0.0.1:1"
		g.config.Health.ServeHTTP(recorder, httpRequest)
		if recorder.status != request.ExpectedStatus || recorder.overflow {
			return errStandaloneUpdateGate
		}
	}
	return nil
}

type boundedResponseRecorder struct {
	header   http.Header
	status   int
	written  int
	limit    int
	overflow bool
}

func (r *boundedResponseRecorder) Header() http.Header { return r.header }
func (r *boundedResponseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *boundedResponseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.written += len(body)
	if r.written > r.limit {
		r.overflow = true
	}
	return len(body), nil
}

func (g *standaloneUpdateGate) purge() {
	now := g.config.Now()
	for id, transaction := range g.transactions {
		if !transaction.Drained && now.Sub(transaction.Created) > 2*time.Hour {
			delete(g.transactions, id)
		}
	}
}

func (g *standaloneUpdateGate) persist() error {
	body, err := json.Marshal(standaloneUpdateGateDisk{Schema: standaloneUpdateGateSchema, Transactions: g.transactions})
	if err != nil {
		return err
	}
	return atomicfile.Write(g.config.StatePath, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func (g *standaloneUpdateGate) load() error {
	body, err := os.ReadFile(g.config.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(body) == 0 || len(body) > 256<<10 {
		return errStandaloneUpdateGate
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var disk standaloneUpdateGateDisk
	if decoder.Decode(&disk) != nil || decoder.Decode(&struct{}{}) != io.EOF || disk.Schema != standaloneUpdateGateSchema || len(disk.Transactions) > 128 {
		return errStandaloneUpdateGate
	}
	for id, transaction := range disk.Transactions {
		request := hostdproto.UpdateGateRequest{Operation: hostdproto.UpdateGateTarget, TransactionID: id, Version: transaction.Version, ManifestSHA256: transaction.Manifest}
		if request.Validate() != nil || transaction.Target != g.target() || transaction.Target.Validate() != nil || transaction.Created.IsZero() || (transaction.Committed || transaction.RolledBack) && transaction.Drained || transaction.Committed && transaction.RolledBack || transaction.Drained && (!transaction.PolicyBound || transaction.WorkloadGeneration == 0 && transaction.ProtectedWorkloads != 0) || !transaction.Drained && !transaction.Committed && !transaction.RolledBack && (transaction.WorkloadGeneration != 0 || transaction.ProtectedWorkloads != 0) {
			return errStandaloneUpdateGate
		}
	}
	g.transactions = disk.Transactions
	return nil
}

var _ hostdproto.UpdateGateHandler = (*standaloneUpdateGate)(nil)

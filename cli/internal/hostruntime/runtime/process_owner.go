//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProcessOwner is the native hostd's sole durable process owner. Feature
// listeners, network services and control-plane clients run in the worker.
type ProcessOwner struct {
	sessions   *session.Manager
	executions *execprocess.Manager
	durable    *store.Store
	status     Host
	listen     string
	token      []byte
	bridge     *workloadbridge.Server
	stop       context.CancelFunc
	done       chan error
}

func NewProductionOwner(ctx context.Context, version string, environ func(string) string) (*ProcessOwner, error) {
	cfg, err := runtimeconfig.FromEnv(version, environ)
	if err != nil {
		return nil, err
	}
	workspace := environ("PAPERBOAT_WORKSPACE_ROOT")
	if workspace == "" {
		workspace, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	if err = validateMachineWorkspace(workspace); err != nil {
		return nil, err
	}
	shell, err := validatedMachineShell(environ("PAPERBOAT_SHELL"))
	if err != nil {
		return nil, err
	}
	environment, err := process.BaseEnvironment(shell)
	if err != nil {
		return nil, err
	}
	tokenPath := filepath.Join(cfg.StateRoot, "agent", "token")
	if _, err = os.Lstat(tokenPath); errors.Is(err, os.ErrNotExist) {
		_, err = writeAgentToken(tokenPath, rand.Reader)
	}
	if err != nil {
		return nil, err
	}
	if _, err = readPersistentAgentToken(tokenPath); err != nil {
		return nil, err
	}
	listen := valueOrRuntime(environ("PAPERBOAT_RUNTIME_LISTEN_ADDRESS"), runtimeport.Primary)
	if !LoopbackAddress(listen) {
		return nil, ErrHostInvalid
	}
	environment = append(environment, "PAPERBOAT_FILE_TRANSFER_ENDPOINT=http://"+listen+"/v1/file-transfers", "PAPERBOAT_FILE_TRANSFER_STAGING_ENDPOINT=http://"+listen+"/v1/local-file-transfers", "PAPERBOAT_RUNTIME_AGENT_TOKEN_FILE="+tokenPath, "PAPERBOAT_WORKSPACE_ROOT="+workspace)
	adapter, err := pty.NewShellAdapter(workspace)
	if err != nil {
		return nil, err
	}
	durable, err := store.Open(ctx, store.Config{Root: cfg.StateRoot})
	if err != nil {
		return nil, err
	}
	resources := cfg.Resources
	if resources == (runtimeconfig.ResourceLimits{}) {
		resources = runtimeconfig.DefaultResources
	}
	sessions, err := session.NewManager(session.ManagerConfig{LaunchContext: func(ctx context.Context, command pty.Command) (session.PTYProcess, error) {
		var err error
		command.Env, err = envinject.Merge(command.Env, session.LaunchEnvironment(ctx))
		if err != nil {
			return nil, err
		}
		return adapter.Start(command)
	}, Random: rand.Reader, HistoryBytes: resources.HistoryBytes, AttachmentBytes: cfg.Limits.PendingOutputBytes, MaxSessions: resources.MaxSessions, MaxAttachments: resources.MaxAttachments, MaxInputDecisions: resources.MaxInputDecisions, TerminationTimeout: 10 * time.Second, TerminationGrace: 2 * time.Second, Store: durable})
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	executions, err := execprocess.NewPersistent(ctx, execprocess.Config{WorkspaceRoot: workspace, BaseEnvironment: environment, MaximumActive: resources.MaxConcurrentOps, MaximumOperations: resources.MaxConcurrentOps * 32, ReplayBytes: int(cfg.Limits.PendingOutputBytes), CancelGrace: 2 * time.Second, Store: durable})
	if err != nil {
		_ = sessions.Shutdown(ctx)
		_ = durable.Close()
		return nil, err
	}
	owner := &ProcessOwner{sessions: sessions, executions: executions, durable: durable, listen: listen}
	owner.status.sessions = sessions
	owner.status.executions = executions
	return owner, nil
}
func (o *ProcessOwner) BindLifecycle(endpoint string, token []byte, fence func() hostdproto.Status, acquire func(string, uint64) (func(), error)) {
	o.token = append([]byte(nil), token...)
	o.bridge = &workloadbridge.Server{Endpoint: endpoint, Token: o.token, Sessions: o.sessions, Executions: &execprocess.RemoteServer{Manager: o.executions, MaximumReaders: 4096}, Fence: fence, AcquireActive: acquire, Ready: make(chan error, 1), MaximumConcurrent: 256}
}
func (o *ProcessOwner) StartHostd(ctx context.Context) error { return nil }
func (o *ProcessOwner) StartBridge(ctx context.Context) error {
	if o.bridge == nil {
		return ErrHostInvalid
	}
	ctx, o.stop = context.WithCancel(ctx)
	o.done = make(chan error, 1)
	go func() { o.done <- o.bridge.Run(ctx) }()
	select {
	case err := <-o.bridge.Ready:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (o *ProcessOwner) ShutdownHostd(ctx context.Context) error {
	var bridgeErr error
	if o.stop != nil {
		o.stop()
		select {
		case bridgeErr = <-o.done:
		case <-ctx.Done():
			bridgeErr = ctx.Err()
		}
	}
	return errors.Join(bridgeErr, o.sessions.Shutdown(ctx), o.executions.Shutdown(ctx), o.durable.Close())
}
func (o *ProcessOwner) WorkloadStatus() hostdproto.WorkloadStatus {
	counts := o.sessions.ResourceCounts()
	executions := o.executions.ActiveSnapshots()
	identities := []string{}
	for _, s := range o.sessions.List() {
		if s.State == session.Running || s.State == session.Restarting {
			identities = append(identities, fmt.Sprintf("terminal:%s:%d", s.ID, s.Generation))
		}
	}
	for _, e := range executions {
		identities = append(identities, "exec:"+e.OperationID)
	}
	sort.Strings(identities)
	fingerprint := strings.Join(identities, "\x00")
	protected := counts["processes"] + uint64(len(executions))
	o.status.workloadMu.Lock()
	defer o.status.workloadMu.Unlock()
	if o.status.workloadGeneration == 0 {
		o.status.workloadGeneration = 1
		o.status.workloadFingerprint = fingerprint
	} else if o.status.workloadFingerprint != fingerprint {
		o.status.workloadGeneration++
		o.status.workloadFingerprint = fingerprint
	}
	return hostdproto.WorkloadStatus{Generation: o.status.workloadGeneration, Protected: protected}
}
func (o *ProcessOwner) UpdateGate() hostdproto.UpdateGateHandler { return o }
func (o *ProcessOwner) HandleUpdateGate(ctx context.Context, r hostdproto.UpdateGateRequest) (hostdproto.UpdateGateResponse, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return hostdproto.UpdateGateResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+o.listen+"/v1/local/runtime-update-gate", bytes.NewReader(body))
	if err != nil {
		return hostdproto.UpdateGateResponse{}, err
	}
	req.Header.Set("X-Paperboat-Owner-Capability", base64.RawURLEncoding.EncodeToString(o.token))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return hostdproto.UpdateGateResponse{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return hostdproto.UpdateGateResponse{}, errStandaloneUpdateGate
	}
	var result hostdproto.UpdateGateResponse
	err = json.NewDecoder(http.MaxBytesReader(nil, res.Body, 64<<10)).Decode(&result)
	return result, err
}
func readPersistentAgentToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || !privateAgentTokenFile(path, info) {
		return "", ErrHostInvalid
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return "", ErrHostInvalid
	}
	return token, nil
}

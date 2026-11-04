//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

// ProcessOwner owns native hostd execution and routes terminals to independent
// native owners. Feature listeners and control-plane clients run in the worker.
type ProcessOwner struct {
	sessions      session.Service
	sessionOwners *sessionowner.Connections
	executions    execprocess.Service
	durable       *store.Store
	status        Host
	listen        string
	token         []byte
	bridge        *workloadbridge.Server
	stop          context.CancelFunc
	done          chan error
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
	if _, err := validatedMachineShell(environ("PAPERBOAT_SHELL")); err != nil {
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
	environment := []string{"PAPERBOAT_FILE_TRANSFER_ENDPOINT=http://" + listen + "/v1/file-transfers", "PAPERBOAT_FILE_TRANSFER_STAGING_ENDPOINT=http://" + listen + "/v1/local-file-transfers", "PAPERBOAT_RUNTIME_AGENT_TOKEN_FILE=" + tokenPath, "PAPERBOAT_WORKSPACE_ROOT=" + workspace}
	durable, err := store.Open(ctx, store.Config{Root: cfg.StateRoot})
	if err != nil {
		return nil, err
	}
	resources := cfg.Resources
	if resources == (runtimeconfig.ResourceLimits{}) {
		resources = runtimeconfig.DefaultResources
	}

	connections, err := productionSessionOwners(ctx, cfg, environ)
	if err != nil {
		return nil, errors.Join(err, durable.Close())
	}
	sessions, err := connections.Service()
	if err != nil {
		connections.Close()
		return nil, errors.Join(err, durable.Close())
	}
	remote, err := connections.Executions()
	if err != nil {
		connections.Close()
		return nil, errors.Join(err, durable.Close())
	}
	executions := hostdExecutions{RemoteManager: remote, environment: environment}
	owner := &ProcessOwner{sessions: sessions, sessionOwners: connections, executions: executions, durable: durable, listen: listen}
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
	o.sessionOwners.Close()
	return errors.Join(bridgeErr, o.durable.Close())
}

// WorkloadStatus reports no protected workloads: commands run in session
// owners, which an update does not replace.
func (o *ProcessOwner) WorkloadStatus() hostdproto.WorkloadStatus {
	return hostdproto.WorkloadStatus{Generation: 1}
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

// OpenSSHStreams asks the active worker how many managed SSH connections it bridges. A worker from
// before this route answers 404, which is an error here.
func (o *ProcessOwner) OpenSSHStreams(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+o.listen+ownerOpenSSHPath, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Paperboat-Owner-Capability", base64.RawURLEncoding.EncodeToString(o.token))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return 0, ErrHostInvalid
	}
	var result ownerOpenSSH
	decoder := json.NewDecoder(http.MaxBytesReader(nil, res.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.SSH < 0 {
		return 0, ErrHostInvalid
	}
	return result.SSH, nil
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

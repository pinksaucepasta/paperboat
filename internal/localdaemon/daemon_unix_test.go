//go:build darwin || linux

package localdaemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	clienttransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

type daemonNativeHTTPOpener struct {
	address string
	closed  atomic.Bool
}

func (o *daemonNativeHTTPOpener) OpenTransferStream(ctx context.Context) (net.Conn, error) {
	if o.closed.Load() {
		return nil, net.ErrClosed
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", o.address)
}
func (o *daemonNativeHTTPOpener) Close() error { o.closed.Store(true); return nil }

type daemonNativeTransferPreparer struct{ opener *daemonNativeHTTPOpener }

func (p daemonNativeTransferPreparer) PrepareNativeFileTransfer(context.Context, resolver.ConnectInfo, string) (tunnel.DirectTransferStreamOpener, error) {
	return p.opener, nil
}

func daemonTestPaths(t *testing.T) localapi.Paths {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "pb-ld-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return localapi.Paths{StateRoot: root, RuntimeRoot: root, SocketPath: filepath.Join(root, "api.sock"), LockPath: filepath.Join(root, "daemon.lock")}
}

func TestProcessLockIsExclusiveAndRejectsUnsafePath(t *testing.T) {
	paths := daemonTestPaths(t)
	first, err := acquireProcessLock(paths.LockPath, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := acquireProcessLock(paths.LockPath, os.Geteuid()); !errors.Is(err, localapi.ErrAlreadyRunning) {
		t.Fatalf("second lock err=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireProcessLock(paths.LockPath, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	unsafeTarget := filepath.Join(paths.StateRoot, "target")
	if err := os.WriteFile(unsafeTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(paths.StateRoot, "symlink.lock")
	if err := os.Symlink(unsafeTarget, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireProcessLock(symlink, os.Geteuid()); !errors.Is(err, localapi.ErrUnsafeSocket) {
		t.Fatalf("symlink lock err=%v", err)
	}
}

func TestDaemonPublishesSnapshotServesAPIAndStopsCleanly(t *testing.T) {
	paths := daemonTestPaths(t)
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", DisplayName: "Studio Mac", Online: true, InstallationGeneration: 4}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, DaemonConfig{Paths: paths, Source: source, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), RefreshInterval: time.Second, RequestTimeout: time.Second})
	}()
	waitForDaemonSocket(t, paths.SocketPath)
	client, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil || snapshot.DaemonState != "ready" || len(snapshot.Machines) != 1 || snapshot.Machines[0].Alias != "Studio Mac" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	now := time.Now().UTC()
	observation := localapi.TransportObservation{Schema: localapi.ObservationSchemaV1, SourceID: "source_canary", Sequence: 1, ObservedAt: now, ExpiresAt: now.Add(15 * time.Second), MachineID: "machine_1", ActiveConsumers: 1, SelectedPath: "relay", RelayRegion: "bom", NATMappingIPv4: "unknown", NATMappingIPv6: "unknown", CaptivePortal: "unknown", PMTU: "unknown", RouterProtocol: "unknown", RouterMapping: "unknown", MappingLifetime: "unknown"}
	if err := client.PublishTransportObservation(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
	snapshot, err = client.Snapshot(context.Background())
	if err != nil || snapshot.Generation != 3 || snapshot.Machines[0].ActiveConsumers != 1 || snapshot.Machines[0].SelectedPath != "relay" || snapshot.Machines[0].RelayRegion != "bom" {
		t.Fatalf("observed snapshot=%#v err=%v", snapshot, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, err := os.Lstat(paths.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket cleanup err=%v", err)
	}
}

func TestDaemonNativeFileTransferIPCResumesLostCommitExactly(t *testing.T) {
	data := bytes.Repeat([]byte("paperboat-native"), (2*protocol.FileTransferChunkBytes)/16+3)
	digest := sha256.Sum256(data)
	manifest := clienttransfer.Manifest{TransferID: "ft_daemon_native", BatchID: "batch_daemon_native", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", SessionID: "session_1", Basename: "payload.bin", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), State: "uploading"}
	var mu sync.Mutex
	var content []byte
	lostResponse := false
	application := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if request.Header.Get("Authorization") != "Bearer file-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/file-transfers":
			_ = json.NewEncoder(writer).Encode(clienttransfer.Batch{BatchID: manifest.BatchID, Transfers: []clienttransfer.Manifest{manifest}})
		case request.Method == http.MethodHead:
			writer.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
		case request.Method == http.MethodPatch:
			offset, _ := strconv.ParseInt(request.Header.Get("Upload-Offset"), 10, 64)
			chunk, err := io.ReadAll(request.Body)
			chunkDigest := sha256.Sum256(chunk)
			if err != nil || offset != int64(len(content)) || len(chunk) > protocol.FileTransferChunkBytes || request.Header.Get("Upload-Digest") != "sha256="+hex.EncodeToString(chunkDigest[:]) {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			content = append(content, chunk...)
			if !lostResponse {
				lostResponse = true
				connection, _, hijackErr := writer.(http.Hijacker).Hijack()
				if hijackErr == nil {
					_ = connection.Close()
				}
				return
			}
			writer.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/complete"):
			manifest.CommittedOffset, manifest.State = int64(len(content)), "published"
			_ = json.NewEncoder(writer).Encode(map[string]any{"transfer": manifest, "result": map[string]string{"code": "published", "path": "Paperboat Inbox/payload.bin"}})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer application.Close()
	opener := &daemonNativeHTTPOpener{address: application.Listener.Addr().String()}
	broker, err := NewFileTransferBroker(daemonNativeTransferPreparer{opener: opener})
	if err != nil {
		t.Fatal(err)
	}
	paths := daemonTestPaths(t)
	source := &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_destination", DisplayName: "Destination", Online: true, InstallationGeneration: 1}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, DaemonConfig{Paths: paths, Source: source, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), RefreshInterval: time.Second, RequestTimeout: time.Second, FileTransfers: broker})
	}()
	waitForDaemonSocket(t, paths.SocketPath)
	local, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Add(time.Minute)
	lease, err := local.PrepareFileTransfer(t.Context(), localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: "machine_destination", EnvironmentID: "environment_1", MachineGeneration: 1, OperationID: "operation_1", Credential: "file-token", AccessSessionID: "access_1", Deadline: deadline, MaximumBytes: uint64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	client, err := clienttransfer.NewNativeClient(application.URL+"/v1/file-transfers", clienttransfer.Auth{Token: "file-token", ExpiresAt: deadline}, clienttransfer.Binding{SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1"}, lease.OpenTransferStream)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := client.SendBatch(t.Context(), manifest.BatchID, manifest.SessionID, []clienttransfer.Source{{Basename: manifest.Basename, Size: int64(len(data)), SHA256: digest, Reader: bytes.NewReader(data)}})
	if err != nil || batch.BatchID != manifest.BatchID {
		t.Fatalf("native send batch=%+v err=%v", batch, err)
	}
	mu.Lock()
	exact := bytes.Equal(content, data)
	mu.Unlock()
	if !lostResponse || !exact {
		t.Fatalf("lost_response=%t exact=%t bytes=%d", lostResponse, exact, len(content))
	}
	_ = client.Close()
	_ = lease.Close()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("daemon stop err=%v", err)
	}
	if !opener.closed.Load() {
		t.Fatal("daemon did not close native transfer association")
	}
}

func TestDaemonOwnsStaleSocketCleanupAfterLockAcquisition(t *testing.T) {
	paths := daemonTestPaths(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.SocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	source := &scriptedMachineSource{results: []machineResult{{err: errors.New("offline")}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, DaemonConfig{Paths: paths, Source: source, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), RefreshInterval: time.Second, RequestTimeout: time.Second})
	}()
	waitForDaemonSocket(t, paths.SocketPath)
	client, _ := localapi.NewClient(paths.SocketPath, time.Second)
	snapshot, err := client.Snapshot(context.Background())
	if err != nil || snapshot.DaemonState != "degraded" || len(snapshot.Health) != 1 || snapshot.Health[0].Code != "control_plane_unavailable" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run err=%v", err)
	}
}

type managedSSHReadinessTestSource struct {
	ready bool
	code  string
}

func (s *managedSSHReadinessTestSource) SetManagedSSHReadiness(ready bool, code string) {
	s.ready = ready
	s.code = code
}

func (s *managedSSHReadinessTestSource) ListUserMachines(context.Context) ([]api.UserMachine, error) {
	return []api.UserMachine{{
		ID: "machine_1", DisplayName: "Studio Mac", Online: true, InstallationGeneration: 4,
		SSHLocalReady: s.ready, SSHLocalCode: s.code,
		SSHAuthority: api.SSHAuthority{TargetGeneration: 4, HostKeyGeneration: 4},
	}}, nil
}

func TestDaemonSurfacesManagedSSHStartupFailureWithoutStopping(t *testing.T) {
	paths := daemonTestPaths(t)
	source := &managedSSHReadinessTestSource{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, DaemonConfig{
			Paths: paths, Source: source, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
			RefreshInterval: time.Second, RequestTimeout: time.Second, ManagedSSH: &ManagedSSHConfig{},
		})
	}()
	waitForDaemonSocket(t, paths.SocketPath)
	client, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil || snapshot.DaemonState != "ready" || len(snapshot.Machines) != 1 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	machine := snapshot.Machines[0]
	if machine.SSHReadiness != "degraded" || len(machine.Health) != 1 || machine.Health[0].Code != "ssh_key_rejected" || machine.Health[0].Recovery != managedSSHDoctorRecovery {
		t.Fatalf("managed SSH failure was not surfaced: %#v", machine)
	}
	diagnostics, err := client.Diagnostics(context.Background())
	if err != nil || len(diagnostics.Recent) < 2 {
		t.Fatalf("diagnostics=%#v err=%v", diagnostics, err)
	}
	startup := diagnostics.Recent[1]
	if startup.Category != "ssh" || startup.Code != "managed_startup" || startup.Severity != "warning" || startup.Fields["outcome"] != "degraded" || startup.Fields["reason"] != "ssh_key_rejected" {
		t.Fatalf("managed SSH startup diagnostic=%#v", startup)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run err=%v", err)
	}
}

func waitForDaemonSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err := localapi.NewClient(path, 20*time.Millisecond)
		if err == nil {
			snapshot, snapshotErr := client.Snapshot(context.Background())
			if snapshotErr == nil && snapshot.DaemonState != "starting" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("daemon socket %s was not ready", path)
}

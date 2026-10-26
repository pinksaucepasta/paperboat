//go:build windows

package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
)

// windowsRegressionAccessSource is intentionally inert. The PAC failure is
// injected before AccessService can bind a listener, so this test exercises
// only hostd lifecycle ownership and never opens a network connection.
type windowsRegressionAccessSource struct{}

func (windowsRegressionAccessSource) Snapshot(context.Context) ([]privatepreviewproxy.AccessRoute, error) {
	return nil, nil
}

func (windowsRegressionAccessSource) Open(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, errors.New("access source must not be opened during PAC startup")
}

type windowsRegressionPAC struct {
	recoverErr error
}

func (p windowsRegressionPAC) Refresh(context.Context, string) error { return nil }
func (p windowsRegressionPAC) Recover(context.Context) error         { return p.recoverErr }
func (windowsRegressionPAC) Install(context.Context, string) error {
	return errors.New("PAC install must not be reached after recovery failure")
}
func (windowsRegressionPAC) Remove(context.Context) error { return nil }

// TestWindowsPACFailureLeavesOwnerSessionLeasesUsable protects the failure
// boundary that was broken on Victus: an interactive-user PAC failure is
// optional in a LocalSystem hostd process and must not tear down the preview
// owner-session manager. Shutdown still closes the manager and rejects later
// acquisition.
func TestWindowsPACFailureLeavesOwnerSessionLeasesUsable(t *testing.T) {
	runtimeDone := make(chan struct{})
	owners, err := preview.NewRuntimeOwnerSessionRegistry(preview.RuntimeOwnerSessionRegistryConfig{
		MachineID: "machine_windows_regression", RuntimeDone: runtimeDone,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerLeases, err := preview.NewOwnerSessionLeaseManager(preview.OwnerSessionLeaseManagerConfig{
		MachineID: "machine_windows_regression", ControlToken: "local-control-token", Registry: owners,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x41}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	privateAccess, err := privatepreviewproxy.NewAccessService(privatepreviewproxy.AccessServiceConfig{
		Proxy:        privatepreviewproxy.AccessProxyConfig{Source: windowsRegressionAccessSource{}},
		Configurator: windowsRegressionPAC{recoverErr: errors.New("interactive user registry unavailable")},
	})
	if err != nil {
		t.Fatal(err)
	}
	assembly := &productionPreviewAssembly{
		privateAccess: privateAccess,
		owners:        owners,
		ownerLeases:   ownerLeases,
		runtimeDone:   runtimeDone,
	}

	if err := assembly.Start(context.Background()); err != nil {
		t.Fatalf("optional Windows PAC failure stopped preview assembly: %v", err)
	}
	lease, err := ownerLeases.Acquire(preview.OwnerSessionLeaseRequest{
		Target: preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:8080"},
	}, "preview-owner-regression-1")
	if err != nil {
		t.Fatalf("owner-session acquisition after isolated PAC failure: %v", err)
	}
	if lease.ID == "" || lease.OwnerSessionID == "" {
		t.Fatalf("incomplete owner-session lease: %#v", lease)
	}

	if err := assembly.Shutdown(context.Background()); err != nil {
		t.Fatalf("assembly shutdown: %v", err)
	}
	if _, err := ownerLeases.Acquire(preview.OwnerSessionLeaseRequest{
		Target: preview.LeaseTarget{Scheme: "http", Address: "127.0.0.1:8080"},
	}, "preview-owner-regression-2"); !errors.Is(err, preview.ErrOwnerSessionLeaseLost) {
		t.Fatalf("acquisition after assembly shutdown = %v, want owner-session lease lost", err)
	}
}

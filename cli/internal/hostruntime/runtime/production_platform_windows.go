//go:build windows

package runtime

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/availability"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	hostruntimeservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
	"golang.org/x/sys/windows"
)

// Compatibility names keep the existing Windows contract tests attached to
// the full production composition rather than the retired reduced runtime.
type windowsConnectorService = dedicatedConnectorService

func windowsWorkspace(environ func(string) string) (string, error) {
	workspace := strings.TrimSpace(environ("PAPERBOAT_WORKSPACE_ROOT"))
	if workspace == "" {
		var err error
		workspace, err = os.UserHomeDir()
		if err != nil {
			return "", errors.Join(ErrProductionInvalid, err)
		}
	}
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return "", ErrProductionInvalid
	}
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrProductionInvalid
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(workspace))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", ErrProductionInvalid
	}
	return workspace, nil
}

func initializeProductionManagedSSH(ctx context.Context, host *managedssh.Host, controlURL string, transport http.RoundTripper, registration runtimeidentity.Registration, identity managedSSHIdentitySource, generation, workerEpoch uint64, keyEvents *machineevents.Subscription) (Service, error) {
	if registration.MachineID == "" || registration.InstallationGeneration < 1 || identity == nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("Windows managed SSH registration is incomplete"))
	}
	if registration.SSHPort == 0 && registration.SSHUser == "" {
		return nil, nil
	}
	if registration.SSHPort == 0 || registration.SSHUser == "" {
		return nil, ErrManagedSSHUnavailable
	}
	user, sidErr := windows.GetCurrentProcessToken().GetTokenUser()
	if sidErr != nil || user == nil || user.User.Sid == nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, sidErr)
	}
	ownerSID := user.User.Sid.String()
	targetEpoch, port := uint64(registration.InstallationGeneration), registration.SSHPort
	var descriptor *windowsopenssh.GenerationDescriptor
	if workerEpoch != 0 {
		executing, source, err := installsource.Current()
		if err != nil {
			return nil, errors.Join(ErrManagedSSHUnavailable, err)
		}
		prepared, err := windowsopenssh.LoadGeneration(ctx, ownerSID, source.SHA256[:32])
		if err != nil {
			return nil, errors.Join(ErrManagedSSHUnavailable, err)
		}
		if err := bindExecutingWindowsSSHGeneration(prepared, source, executing); err != nil {
			return nil, err
		}
		descriptor = &prepared
		targetEpoch, port = workerEpoch, prepared.Port
	}
	var err error
	// PaperboatSshd is an SCM-managed service and can still be binding its
	// loopback sockets while the owner-scoped host supervisor starts. A single
	// probe here turns that normal startup race into a permanent stopped hostd.
	// Retry only within a bounded startup window; genuine failures still fail
	// closed and are reported to the control plane.
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		err = nil
		if descriptor != nil {
			err = windowsopenssh.InspectGeneration(probeCtx, *descriptor)
			if errors.Is(err, windowsopenssh.ErrServiceUnhealthy) {
				err = errors.Join(managedssh.ErrSSHTargetUnavailable, err)
			}
		}
		if err == nil {
			if descriptor != nil {
				_, err = host.ReconcileSourceTarget(probeCtx, targetEpoch, port, descriptor.Source.SHA256)
			} else {
				_, err = host.ReconcileTarget(probeCtx, targetEpoch, port)
			}
		}
		if err == nil || !errors.Is(err, managedssh.ErrSSHTargetUnavailable) {
			break
		}
		retry := time.NewTimer(250 * time.Millisecond)
		select {
		case <-probeCtx.Done():
			retry.Stop()
			err = errors.Join(managedssh.ErrSSHTargetUnavailable, probeCtx.Err())
		case <-retry.C:
		}
		if probeCtx.Err() != nil {
			err = errors.Join(managedssh.ErrSSHTargetUnavailable, probeCtx.Err())
			break
		}
	}
	if errors.Is(err, managedssh.ErrSSHTargetUnavailable) {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("PaperboatSshd loopback target is unavailable"), err)
	}
	if err != nil {
		return nil, err
	}
	instance, instanceErr := hostruntimeservice.WindowsUserInstance(ownerSID)
	if instanceErr != nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, instanceErr)
	}
	instanceRoot, rootErr := hostinstall.WindowsInstanceRoot(instance)
	if rootErr != nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, rootErr)
	}
	sshStateRoot := filepath.Join(instanceRoot, "ssh")
	paths := []string{filepath.Join(sshStateRoot, "hostkeys", "ssh_host_ed25519_key.pub")}
	inventory, err := managedssh.ReadHostPublicKeys(paths, 0)
	if err != nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("read Windows Paperboat host keys"), err)
	}
	if len(inventory.Keys) == 0 || generation == 0 {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("Windows Paperboat host-key inventory is empty"))
	}
	publicKeys := make([]string, len(inventory.Keys))
	for i := range inventory.Keys {
		publicKeys[i] = inventory.Keys[i].PublicKey
	}
	client := api.New(controlURL, config.Credential{}, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	return &managedSSHKeyReconciler{client: client, identity: identity, registration: registration, workerGeneration: generation, publicKeys: publicKeys, home: sshStateRoot, interval: 30 * time.Second, timeout: 10 * time.Second, events: keyEvents}, nil
}

func validatedMachineShell(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		path = os.Getenv("ComSpec")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", ErrProductionInvalid
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrProductionInvalid
	}
	return path, nil
}

func validateMachineWorkspace(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrProductionInvalid
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrProductionInvalid
	}
	return nil
}

func newProductionAvailabilityHostClient(timeout time.Duration) (*availability.HostClient, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	instance, err := hostruntimeservice.WindowsUserInstance(user.User.Sid.String())
	if err != nil {
		return nil, err
	}
	socket, err := hostservice.WindowsSocketPath(instance)
	if err != nil {
		return nil, err
	}
	return availability.NewHostClient(socket, timeout)
}

func cleanupProductionManagedSSH(ctx context.Context, _ runtimeidentity.Registration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return ErrManagedSSHUnavailable
	}
	instance, err := hostruntimeservice.WindowsUserInstance(user.User.Sid.String())
	if err != nil {
		return err
	}
	root, err := hostinstall.WindowsInstanceRoot(instance)
	if err != nil {
		return err
	}
	_, err = reconcilePlatformAuthorizedKeys(filepath.Join(root, "ssh"), 0, nil)
	return err
}

// AutomaticUpdates is installation policy rather than executing-byte identity.
// Every source identity field and the immutable executable path must match;
// neither installation Source nor its old SSH port can select this generation.
func bindExecutingWindowsSSHGeneration(d windowsopenssh.GenerationDescriptor, source installsource.Source, executable string) error {
	if d.Validate() != nil || source.Validate() != nil || d.Source.SHA256 != source.SHA256 || d.Source.Length != source.Length || d.Source.Platform != source.Platform || d.Source.Architecture != source.Architecture || d.Source.Version != source.Version || d.Source.Distribution != source.Distribution || !strings.EqualFold(filepath.Clean(d.Executable), filepath.Clean(executable)) {
		return ErrManagedSSHUnavailable
	}
	return d.Source.Verify(executable)
}

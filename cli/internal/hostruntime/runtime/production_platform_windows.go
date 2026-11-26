//go:build windows

package runtime

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/availability"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostservice"
	runtimeidentity "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	hostruntimeservice "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/cli/internal/windowsopenssh"
	"golang.org/x/sys/windows"
)

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
	user, sidErr := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
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
		layout, err := hostruntimeservice.WindowsUserLayout(ownerSID)
		if err != nil {
			return nil, errors.Join(ErrManagedSSHUnavailable, err)
		}
		if err := bindExecutingWindowsSSHGeneration(layout.ReleasesRoot, prepared, source, executing); err != nil {
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
	hostKeyDir := filepath.Join(sshStateRoot, "hostkeys")
	// Re-read on every change: pb doctor --repair recreates deleted host keys while hostd keeps running, and the
	// server must learn the new key (Unix does the same with its /etc/ssh watch).
	readPublicKeys := windowsHostPublicKeys(hostKeyDir)
	publicKeys, err := readPublicKeys()
	if err != nil {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("read Windows Paperboat host keys"), err)
	}
	if len(publicKeys) == 0 || generation == 0 {
		return nil, errors.Join(ErrManagedSSHUnavailable, errors.New("Windows Paperboat host-key inventory is empty"))
	}
	watchHostKeys := func(ctx context.Context) (<-chan struct{}, error) {
		return watchHostKeyFiles(ctx, hostKeyDir, sshHostKeyQuiet)
	}
	client := api.New(controlURL, config.Credential{}, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	return &managedSSHKeyReconciler{host: host, client: client, identity: identity, registration: registration, workerGeneration: generation, publicKeys: publicKeys, readPublicKeys: readPublicKeys, watchHostKeys: watchHostKeys, home: sshStateRoot, interval: 30 * time.Second, timeout: 10 * time.Second, events: keyEvents}, nil
}

// windowsHostPublicKeys reads the managed sshd's host public key from dir, at every call.
func windowsHostPublicKeys(dir string) func() ([]string, error) {
	return func() ([]string, error) {
		inventory, err := managedssh.ReadHostPublicKeys([]string{filepath.Join(dir, "ssh_host_ed25519_key.pub")}, 0)
		if err != nil {
			return nil, err
		}
		publicKeys := make([]string, len(inventory.Keys))
		for i := range inventory.Keys {
			publicKeys[i] = inventory.Keys[i].PublicKey
		}
		return publicKeys, nil
	}
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
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
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
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
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
	_, err = reconcilePlatformAuthorizedKeys(filepath.Join(root, "ssh"), 0, nil, "", nil)
	return err
}

// bindExecutingWindowsSSHGeneration accepts the SSH generation prepared for the bytes this worker runs, from one of
// that artifact's two pinned paths under releasesRoot (versions\<version> or versions\custom-<sha>). The generation
// keeps the path and update policy of the install that first prepared it (PrepareGeneration reuses it for the same
// artifact), so `pb update --from` of the installed bytes runs this worker from the other path.
func bindExecutingWindowsSSHGeneration(releasesRoot string, d windowsopenssh.GenerationDescriptor, source installsource.Source, executable string) error {
	if d.Validate() != nil || !d.Source.SameArtifact(source) {
		return ErrManagedSSHUnavailable
	}
	for _, key := range []string{d.Source.Version, "custom-" + d.Source.SHA256} {
		if strings.EqualFold(filepath.Clean(executable), filepath.Join(releasesRoot, "versions", key, "pb.exe")) {
			return d.Source.Verify(executable)
		}
	}
	return ErrManagedSSHUnavailable
}

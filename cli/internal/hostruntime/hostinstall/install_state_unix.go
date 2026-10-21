//go:build darwin || linux

package hostinstall

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

type installJournal struct {
	PreviousMetadata []byte               `json:"previous_metadata"`
	MetadataCaptured bool                 `json:"metadata_captured"`
	Source           installsource.Source `json:"source"`
	Schema           string               `json:"schema"`
	Stage            string               `json:"stage"`
	HadWorker        bool                 `json:"had_worker"`
	UpdatedAt        time.Time            `json:"updated_at"`
}

func newInstallJournal(paths installPaths, source installsource.Source) (installJournal, error) {
	hadWorker, probeErr := checkRegularFile(paths.worker)
	if probeErr != nil && !installNotExistOnly(probeErr) {
		return installJournal{}, probeErr
	}
	journal := installJournal{Source: source, Schema: journalSchemaV1, Stage: "prepared", HadWorker: hadWorker, UpdatedAt: time.Now().UTC(), MetadataCaptured: true}
	info, err := os.Lstat(paths.metadata)
	if installNotExistOnly(err) {
		return journal, nil
	}
	if err != nil {
		return installJournal{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || ownerUID(info) != 0 || info.Size() > 128<<10 {
		return installJournal{}, ErrInvalidRequest
	}
	journal.PreviousMetadata, err = readProtectedInstallState(paths.metadata, 128<<10)
	return journal, err
}

func platformPaths(uid int) installPaths {
	layout, err := service.UserLayout(runtime.GOOS, uid)
	if err != nil {
		panic(hostInstallFailure("unsupported Unix service layout", err))
	}
	root := layout.InstallRoot
	instance := "u" + strconv.Itoa(uid)
	installerState, runtimeState := filepath.Join("/var/lib/paperboat-installer/users", instance), filepath.Join("/var/lib/paperboat/users", instance)
	if runtime.GOOS == "darwin" {
		installerState = filepath.Join("/Library/Application Support/Paperboat/users", instance)
		runtimeState = installerState
	}
	p := installPaths{instance: instance, root: root, installerState: installerState, runtimeState: runtimeState}
	p.worker = layout.Binary
	// Release slots are kept in a dedicated root-owned directory so the
	// updater can atomically stage, promote, and roll back without touching the
	// command path or relying on a caller-controlled location.
	releases := layout.ReleasesRoot
	p.workerNext = layout.BinaryStaged
	p.workerRollback = layout.BinaryRollback
	p.workerPrevious = filepath.Join(releases, "pb.previous")
	p.journal = filepath.Join(installerState, "install-journal.json")
	p.metadata = filepath.Join(installerState, "install-metadata.json")
	p.hostdToken = filepath.Join(runtimeState, "hostd.token")
	p.hostdSocket = layout.HostdSocket
	p.updaterSocket = layout.UpdaterSocket
	p.updateState = layout.UpdateStateRoot
	if runtime.GOOS != "darwin" {
		p.environmentCredentialDirectory = filepath.Join(installerState, "environment")
		p.environmentCredential = filepath.Join(p.environmentCredentialDirectory, "host-key.cred")
		p.environmentCredentialMetadata = filepath.Join(p.environmentCredentialDirectory, "host-key.json")
	}
	return p
}

// LoadEnrolledOwner returns the committed, root-protected installation
// identity for the requested Unix account. The privileged updater uses this
// to launch owner-scoped readiness probes without trusting caller input.
func LoadEnrolledOwner(uid int) (Request, error) {
	if os.Geteuid() != 0 {
		return Request{}, ErrNotPrivileged
	}
	return loadInstallMetadata(platformPaths(uid).metadata, uid)
}

func ensureHostdToken(paths installPaths, request Request) error {
	// Hostd runs as the enrolled user, while the token directory is root-owned.
	// Keep the directory non-writable to that user but traversable so hostd can
	// read its 0600 token file.
	if err := secureRootDirectory(filepath.Dir(paths.hostdToken), 0o755); err != nil {
		return err
	}
	info, err := os.Lstat(paths.hostdToken)
	if installNotExistOnly(err) {
		token := make([]byte, 32)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		if err := os.WriteFile(paths.hostdToken, token, 0o600); err != nil {
			return err
		}
	} else if err != nil {
		return hostInstallFailure("inspect existing runtime token", err)
	} else if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return ErrInvalidRequest
	}
	if err := os.Chown(paths.hostdToken, request.UID, request.GID); err != nil {
		return err
	}
	return os.Chmod(paths.hostdToken, 0o600)
}

func ensureSharedUserParents(paths installPaths) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	users := filepath.Dir(paths.runtimeState)
	root := filepath.Dir(users)
	for _, path := range []string{root, users} {
		if err := secureRootDirectory(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func ensureManagedDirectories(paths installPaths, request Request) error {
	// hostd runs as the enrolled account and must be able to create its
	// authenticated socket and fence state. The updater remains root-owned.
	for _, directory := range []string{filepath.Dir(paths.hostdSocket), filepath.Join(paths.runtimeState, "hostd")} {
		if err := secureManagedUserDirectory(directory, 0o700, request.UID, request.GID); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Dir(paths.updaterSocket), 0o755},
		{paths.updateState, 0o700},
		{filepath.Join(paths.root, "releases"), 0o755},
		{filepath.Dir(paths.worker), 0o755},
	} {
		if err := secureRootDirectory(item.path, item.mode); err != nil {
			return err
		}
	}
	return nil
}

func ensureEnvironmentHostCredential(ctx context.Context, paths installPaths, request Request) (bool, error) {
	if request.Platform != "linux" {
		return false, nil
	}
	if request.InstallationGeneration < 1 {
		return false, ErrInvalidRequest
	}
	return environmentkey.EnsureSystemdCredential(ctx, environmentkey.ProvisionConfig{
		CiphertextPath: paths.environmentCredential,
		MachineID:      request.UserMachineID, Generation: uint64(request.InstallationGeneration),
	})
}

// secureManagedUserDirectory accepts the two owners that can safely have
// created a runtime socket directory: root from a prior installation, or the
// enrolled account from a running hostd. It rejects every other owner and
// never follows symlinks before transferring ownership to the enrolled user.
func secureManagedUserDirectory(path string, mode os.FileMode, uid, gid int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if installNotExistOnly(err) {
		if err := os.MkdirAll(path, mode); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return ErrInvalidRequest
	}
	owner := ownerUID(info)
	if owner != 0 && owner != uid {
		return ErrInvalidRequest
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func writeInstallMetadata(path string, request Request) error {
	if err := secureRootDirectory(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: os.Geteuid(), OwnerGID: -1})
}

func loadInstallMetadata(path string, sudoUID int) (Request, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Request{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || ownerUID(info) != 0 || info.Size() < 1 || info.Size() > 128<<10 {
		return Request{}, ErrInvalidRequest
	}
	body, err := readProtectedInstallState(path, 128<<10)
	if err != nil {
		return Request{}, err
	}
	request, err := Decode(strings.NewReader(string(body)))
	if err != nil {
		return Request{}, hostInstallFailure("reading installation state failed", ErrInvalidRequest, err)
	}
	if request.Schema != SchemaV1 || request.Platform != runtime.GOOS || !validRunIdentity(request) || request.UID != sudoUID {
		return Request{}, ErrInvalidRequest
	}
	account, err := user.Lookup(request.User)
	if err != nil {
		return Request{}, hostInstallFailure("reading installation state failed", ErrInvalidRequest, err)
	}
	if account.Uid != strconv.Itoa(request.UID) || account.Gid != strconv.Itoa(request.GID) || account.HomeDir != request.Home {
		return Request{}, ErrInvalidRequest
	}
	group, err := user.LookupGroup(request.Group)
	if err != nil {
		return Request{}, hostInstallFailure("reading installation state failed", ErrInvalidRequest, err)
	}
	if group.Gid != strconv.Itoa(request.GID) {
		return Request{}, ErrInvalidRequest
	}
	if !request.EnrollmentPending && request.InstallationGeneration < 1 {
		return Request{}, ErrInvalidRequest
	}
	return request, nil
}

func stageBinary(source, destination string, identity installsource.Source) (resultErr error) {
	if err := identity.Verify(source); err != nil {
		return err
	}
	if err := secureRootDirectory(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("installation cleanup failed", resultErr, input.Close()) }()
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=streamed-binary-staging
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".binary-*")
	if diskFull(err) {
		return hostInstallFailure("staging installation failed: no free disk space", ErrInsufficientSpace, err)
	}
	if err != nil {
		return err
	}
	path := temporary.Name()
	defer func() {
		resultErr = withCleanup("staged installation cleanup failed", resultErr, removeInstallFile(path))
	}()
	if err := temporary.Chmod(0o755); err != nil {
		return hostInstallFailure("staging installation failed", err, temporary.Close())
	}
	written, err := io.Copy(temporary, io.LimitReader(input, 256<<20+1))
	if diskFull(err) {
		return hostInstallFailure("staging installation failed: no free disk space", ErrInsufficientSpace, err, temporary.Close())
	}
	if err != nil || written < 1 || written > 256<<20 {
		return hostInstallFailure("staging installation failed", ErrInvalidRequest, err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		if diskFull(err) {
			return hostInstallFailure("staging installation failed: no free disk space", ErrInsufficientSpace, err, temporary.Close())
		}
		return hostInstallFailure("staging installation failed", err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		if diskFull(err) {
			return hostInstallFailure("staging installation failed: no free disk space", ErrInsufficientSpace, err)
		}
		return err
	}
	if err := identity.Verify(path); err != nil {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=verified-stage-publication
	if err := os.Rename(path, destination); err != nil {
		return err
	}
	return nil
}

// activatePreparedCandidate moves the staged candidate into the fixed worker slot. The staged file
// is consumed by the move, so the returned request names the installed worker: every later step
// (session-owner and carrier pins) copies from it, never from the staging path that no longer exists.
func activatePreparedCandidate(request Request, paths installPaths) (Request, error) {
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=verified-binary-activation
	if err := os.Rename(paths.workerNext, paths.worker); err != nil {
		return request, err
	}
	request.Executable = paths.worker
	return request, nil
}

func activateBinary(current, next, rollback string) error {
	if err := os.Remove(rollback); err != nil && !installNotExistOnly(err) {
		return err
	}
	if info, err := os.Lstat(current); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || ownerUID(info) != 0 {
			return ErrInvalidRequest
		}
		//paperboat:allow-source-policy atomic-replacement owner=host-install reason=current-binary-rollback-stage
		if err := os.Rename(current, rollback); err != nil {
			return err
		}
	} else if !installNotExistOnly(err) {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=verified-binary-activation
	return os.Rename(next, current)
}

func rollbackFiles(paths installPaths, journal installJournal) error {
	var result error
	for _, item := range []struct {
		current, rollback string
		had               bool
	}{{paths.worker, paths.workerRollback, journal.HadWorker}} {
		rollbackExists, probeErr := checkRegularFile(item.rollback)
		if probeErr != nil && !installNotExistOnly(probeErr) {
			result = hostInstallFailure("installation rollback inspection failed", result, probeErr)
			continue
		}
		if rollbackExists || !item.had {
			if err := os.Remove(item.current); err != nil && !installNotExistOnly(err) {
				result = errors.Join(result, err)
			}
		}
		if item.had && rollbackExists {
			//paperboat:allow-source-policy atomic-replacement owner=host-install reason=journaled-install-rollback
			if err := os.Rename(item.rollback, item.current); err != nil {
				result = errors.Join(result, err)
			}
		} else if !item.had {
			result = hostInstallFailure("installation rollback cleanup failed", result, removeInstallFile(item.rollback))
		}
	}
	result = hostInstallFailure("installation rollback cleanup failed", result, removeInstallFile(paths.workerNext))
	if result != nil {
		return result
	}
	if journal.MetadataCaptured {
		if len(journal.PreviousMetadata) > 0 {
			if err := atomicfile.Write(paths.metadata, journal.PreviousMetadata, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: -1}); err != nil {
				return err
			}
		} else if err := os.Remove(paths.metadata); err != nil && !installNotExistOnly(err) {
			return err
		}
	}
	if err := updated.RollbackUnixNativeInstall(paths.updateState); err != nil {
		return err
	}
	return removeJournal(paths.journal)
}

func recoverInterrupted(paths installPaths) (bool, error) {
	journal, err := loadJournal(paths.journal)
	if installNotExistOnly(err) {
		return false, updated.CommitUnixNativeInstall(paths.updateState)
	}
	if err != nil {
		return false, err
	}
	return true, rollbackFiles(paths, journal)
}

func writeJournal(path string, journal installJournal) error {
	if err := secureRootDirectory(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: os.Geteuid(), OwnerGID: -1})
}

func loadJournal(path string) (installJournal, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return installJournal{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 256<<10 || ownerUID(info) != 0 {
		return installJournal{}, ErrInvalidRequest
	}
	body, err := readProtectedInstallState(path, 256<<10)
	if err != nil {
		return installJournal{}, err
	}
	var journal installJournal
	if err := decodeInstallJSON(strings.NewReader(string(body)), &journal, 256<<10); err != nil {
		return installJournal{}, err
	}
	if journal.Schema != journalSchemaV1 || !slices.Contains([]string{"prepared", "binaries_activated", "services_started"}, journal.Stage) {
		return installJournal{}, ErrInvalidRequest
	}
	return journal, nil
}

func replacePrevious(rollback, previous string) error {
	if _, err := os.Lstat(rollback); installNotExistOnly(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.Remove(previous); err != nil && !installNotExistOnly(err) {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=host-install reason=verified-previous-retention
	return os.Link(rollback, previous)
}

func removeJournal(path string) (resultErr error) {
	err := os.Remove(path)
	if installNotExistOnly(err) {
		return nil
	}
	if err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("installation cleanup failed", resultErr, directory.Close()) }()
	return directory.Sync()
}

func removeInstalledFiles(ctx context.Context, paths installPaths) error {
	keep, _ := ctx.Value(resetCLIContextKey{}).(string)
	var result error
	carrierSocket := ""
	if paths.hostdSocket != "" {
		carrierSocket = paths.hostdSocket + ".carrier"
	}
	for _, path := range []string{
		paths.worker, paths.workerNext, paths.workerRollback, paths.workerPrevious,
		paths.metadata,
		paths.hostdToken, paths.hostdSocket, carrierSocket, paths.updaterSocket,
		filepath.Join(paths.runtimeState, "hostd", "fence.json"),
		filepath.Join(paths.runtimeState, "hostd"),
		paths.environmentCredential, paths.environmentCredentialMetadata,
		filepath.Join(paths.runtimeState, "power-baseline.json"),
		filepath.Join(paths.runtimeState, "availability-policy.json"),
		filepath.Join(paths.runtimeState, "update-current.json"),
		filepath.Join(paths.runtimeState, "update-journal.json"),
		filepath.Join(paths.runtimeState, "update-rollbacks.json"),
		filepath.Join(paths.runtimeState, "privileged-updates", "update-current.json"),
		filepath.Join(paths.runtimeState, "privileged-updates", "update-journal.json"),
		filepath.Join(paths.runtimeState, "privileged-updates", "update-rollbacks.json"),
		filepath.Join(paths.runtimeState, "privileged-updates"),
	} {
		if path == "" || path == keep {
			continue
		}
		if err := os.Remove(path); err != nil && !installNotExistOnly(err) {
			result = errors.Join(result, err)
		}
	}
	for _, directory := range []string{paths.environmentCredentialDirectory, paths.updateState, filepath.Dir(paths.updaterSocket), filepath.Join(paths.root, "releases")} {
		if directory == "" || directory == "." || directory == string(filepath.Separator) || !filepath.IsAbs(directory) {
			continue
		}
		if err := RemovePathPreservingCLI(ctx, directory, keep); err != nil {
			result = errors.Join(result, err)
		}
	}
	// Preserved account-private control directories are removed only after
	// every owning native role is uninstalled. Never recursively delete an
	// unexpected entry in this shared lifetime namespace.
	if runtime.GOOS == "darwin" && paths.hostdSocket != "" {
		if err := os.RemoveAll(filepath.Dir(paths.hostdSocket)); err != nil {
			result = errors.Join(result, err)
		}
	}
	if runtime.GOOS == "linux" && paths.hostdSocket != "" {
		uid, err := carrierUID(paths.instance)
		if err != nil {
			return errors.Join(result, err)
		}
		if paths.hostdSocket != platformPaths(uid).hostdSocket {
			return errors.Join(result, ErrInvalidRequest)
		}
		for _, directory := range []string{filepath.Dir(paths.hostdSocket), filepath.Join("/run", "paperboat-carrier-"+paths.instance)} {
			info, err := os.Lstat(directory)
			if installNotExistOnly(err) {
				continue
			}
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || ownerUID(info) != uid {
				result = errors.Join(result, ErrInvalidRequest)
				continue
			}
			if err := os.Remove(directory); err != nil && !installNotExistOnly(err) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

func regularFile(path string) bool {
	regular, err := checkRegularFile(path)
	return err == nil && regular
}

func checkRegularFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, ErrInvalidRequest
	}
	return true, nil
}

// lockUnixInstallState owns the directory required before activation locking.
func lockUnixInstallState(ctx context.Context, root string) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInvalidRequest
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		// Validate custody before creating only the lock infrastructure. A shared
		// sticky system temp ancestor is safe when its descendant is root-owned.
		for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
			info, err := os.Lstat(parent)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || ownerUID(info) != 0 || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
				return nil, ErrInvalidRequest
			}
			if parent == string(filepath.Separator) {
				break
			}
		}
		if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	// Existing entries remain subject to the activation owner's strict checks;
	// never chmod or repair unexpected state to obtain admission.
	return updated.LockUnixNativeInstall(ctx, root)
}

func secureRootDirectory(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || ownerUID(info) != 0 {
		return ErrInvalidRequest
	}
	return os.Chmod(path, mode)
}

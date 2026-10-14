//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"golang.org/x/sys/unix"
)

func carrierUID(instance string) (int, error) {
	if !strings.HasPrefix(instance, "u") {
		return 0, ErrInvalidRequest
	}
	uid, err := strconv.Atoi(instance[1:])
	if err != nil || uid < 0 || instance != "u"+strconv.Itoa(uid) {
		return 0, ErrInvalidRequest
	}
	return uid, nil
}
func carrierConfigPath(instance string) string {
	uid, err := carrierUID(instance)
	if err != nil {
		return ""
	}
	return filepath.Join(platformPaths(uid).root, "carrier", "config.json")
}
func carrierServiceRoot(CarrierConfig) string { return "/" }
func validateCarrierPaths(c CarrierConfig) error {
	uid, err := carrierUID(c.Instance)
	if err != nil || uid != c.UID || c.GID < 0 || c.OwnerSID != "" {
		return ErrInvalidRequest
	}
	paths := platformPaths(uid)
	root := filepath.Dir(carrierConfigPath(c.Instance))
	if c.Executable != filepath.Join(filepath.Dir(paths.workerRollback), "pins", c.Source.SHA256, "pb") || c.HostdEndpoint != paths.hostdSocket || c.HostdTokenFile != paths.hostdToken || c.DispatchCapabilityFile != filepath.Join(root, "dispatch.cap") || c.LockPath != filepath.Join(root, "owner.lock") {
		return ErrInvalidRequest
	}
	return nil
}
func prepareCarrierConfig(ctx context.Context, r Request) (CarrierConfig, error) {
	if os.Geteuid() != 0 {
		return CarrierConfig{}, ErrNotPrivileged
	}
	if r.Source.Validate() != nil || r.Source.Platform != r.Platform || r.Source.Platform != runtime.GOOS || r.Source.Architecture != runtime.GOARCH || r.UID < 0 || r.GID < 0 || r.EnrollmentPending {
		return CarrierConfig{}, ErrInvalidRequest
	}
	account, err := user.Lookup(r.User)
	if err != nil || account.Uid != strconv.Itoa(r.UID) || account.Gid != strconv.Itoa(r.GID) {
		return CarrierConfig{}, ErrInvalidRequest
	}
	group, err := user.LookupGroup(r.Group)
	if err != nil || group.Gid != strconv.Itoa(r.GID) {
		return CarrierConfig{}, ErrInvalidRequest
	}
	paths := platformPaths(r.UID)
	root := filepath.Dir(carrierConfigPath(paths.instance))
	pin := filepath.Join(filepath.Dir(paths.workerRollback), "pins", r.Source.SHA256, "pb")
	if _, err := os.Lstat(pin); installNotExistOnly(err) {
		if err := stageBinary(r.Executable, pin, r.Source); err != nil {
			return CarrierConfig{}, err
		}
	} else if err != nil {
		return CarrierConfig{}, err
	}
	if err := r.Source.Verify(pin); err != nil {
		return CarrierConfig{}, err
	}
	c := CarrierConfig{Schema: carrierSchema, Instance: paths.instance, Source: r.Source, Executable: pin, UID: r.UID, GID: r.GID, User: r.User, Group: r.Group, RuntimeRoot: r.StateRoot, Environment: workerEnvironment(r), WorkerAddress: r.HelperListenAddress, HostdEndpoint: paths.hostdSocket, ControlEndpoint: paths.hostdSocket + ".carrier", HostdTokenFile: paths.hostdToken, DispatchCapabilityFile: filepath.Join(root, "dispatch.cap"), LockPath: filepath.Join(root, "owner.lock")}
	return c, c.Validate()
}
func readCarrierConfigFile(ctx context.Context, instance string) (CarrierConfig, error) {
	if ctx == nil {
		return CarrierConfig{}, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return CarrierConfig{}, err
	}
	uid, err := carrierUID(instance)
	if err != nil {
		return CarrierConfig{}, err
	}
	if os.Geteuid() != 0 && os.Geteuid() != uid {
		return CarrierConfig{}, ErrInvalidRequest
	}
	path := carrierConfigPath(instance)
	f, err := openCarrierProtectedFile(CarrierConfig{UID: uid, GID: -1}, path, false)
	if err != nil {
		return CarrierConfig{}, err
	}
	body, err := io.ReadAll(io.LimitReader(f, (32<<10)+1))
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return CarrierConfig{}, err
	}
	c, err := decodeCarrier(body)
	if err != nil {
		return c, err
	}
	if c.Instance != instance {
		return c, ErrInvalidRequest
	}
	verified, err := openCarrierProtectedFile(c, path, false)
	if err != nil {
		return c, err
	}
	if err = verified.Close(); err != nil {
		return c, err
	}
	return c, c.Validate()
}
func openCarrierProtectedFile(c CarrierConfig, path string, writable bool) (*os.File, error) {
	parent, err := openProtectedPinDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	mode := uint32(0600)
	owner := c.UID
	if filepath.Base(path) == "config.json" {
		mode = 0640
		owner = 0
	}
	if writable {
		flags = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	}
	fd, err := unix.Openat(parent, filepath.Base(path), flags, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != owner || st.Nlink != 1 || uint32(st.Mode)&0777 != mode || c.GID >= 0 && int(st.Gid) != c.GID {
		f.Close()
		return nil, ErrInvalidRequest
	}
	return f, nil
}
func publishCarrierFiles(c CarrierConfig, path string, capability []byte) error {
	root := filepath.Dir(path)
	parent, err := openProtectedPinDirectory(filepath.Dir(root))
	if err != nil {
		return err
	}
	unix.Close(parent)
	if err := secureRootDirectory(root, 0755); err != nil {
		return err
	}
	if err := os.Chown(root, 0, c.GID); err != nil {
		return err
	}
	for _, item := range []struct {
		path string
		body []byte
		mode os.FileMode
	}{{c.DispatchCapabilityFile, capability, 0600}, {c.LockPath, []byte{}, 0600}} {
		if _, err := os.Lstat(item.path); err == nil {
			f, err := openCarrierProtectedFile(c, item.path, item.path == c.LockPath)
			if err != nil {
				return err
			}
			if err = f.Close(); err != nil {
				return err
			}
			continue
		} else if !installNotExistOnly(err) {
			return err
		}
		if err := atomicfile.Write(item.path, item.body, atomicfile.Options{Mode: item.mode, OwnerUID: c.UID, OwnerGID: c.GID}); err != nil {
			return err
		}
	}
	body, err := jsonCarrier(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0640, OwnerUID: 0, OwnerGID: c.GID})
}
func verifyCarrierNative(c CarrierConfig, i *service.Installer) error {
	if err := verifyCarrierNativeCustody(c, i); err != nil {
		return err
	}
	return i.VerifyPublishedDefinition()
}
func verifyCarrierNativeCustody(c CarrierConfig, i *service.Installer) error {
	pinParent, err := openProtectedPinDirectory(filepath.Dir(c.Executable))
	if err != nil {
		return err
	}
	unix.Close(pinParent)
	pinInfo, err := os.Lstat(c.Executable)
	if err != nil {
		return err
	}
	if ownerUID(pinInfo) != 0 || !pinInfo.Mode().IsRegular() || pinInfo.Mode().Perm()&0022 != 0 {
		return ErrInvalidRequest
	}

	parent, err := openProtectedPinDirectory(filepath.Dir(i.DefinitionPath()))
	if err != nil {
		return err
	}
	unix.Close(parent)
	info, err := os.Lstat(i.DefinitionPath())
	if err != nil {
		return err
	}
	if ownerUID(info) != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return ErrInvalidRequest
	}
	return nil
}

// LockCarrier holds the account-private lock for the entire carrier lifetime.
// Its root-owned parent prevents the enrolled account from replacing its inode.
func LockCarrier(c CarrierConfig) (io.Closer, error) {
	if c.Validate() != nil || os.Geteuid() != c.UID {
		return nil, ErrInvalidRequest
	}
	f, err := openCarrierProtectedFile(c, c.LockPath, true)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func carrierPublicationLock(ctx context.Context, r Request) (io.Closer, error) {
	if os.Geteuid() != 0 {
		return nil, ErrNotPrivileged
	}
	if r.UID < 0 {
		return nil, ErrInvalidRequest
	}
	return updated.LockUnixNativeInstall(ctx, platformPaths(r.UID).updateState)
}

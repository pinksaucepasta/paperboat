//go:build linux || darwin

package deviceguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func requirePrivilege() error {
	if os.Geteuid() != 0 {
		return errors.New("device guard must run as root")
	}
	return nil
}
func lockState(path string) (io.Closer, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errGuardRunning
		}
		return nil, err
	}
	return file, nil
}
func protectedDirectory(path string, mode os.FileMode) error {
	return protectedDirectoryPath(path, mode, false)
}
func protectedDirectoryPath(path string, mode os.FileMode, allowStickyLeaf bool) error {
	if !filepath.IsAbs(path) {
		return errors.New("device guard directory must be absolute")
	}
	path = filepath.Clean(path)
	current := string(filepath.Separator)
	rootInfo, err := os.Lstat(current)
	if err != nil {
		return err
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || rootStat.Uid != 0 || !rootInfo.IsDir() || rootInfo.Mode().Perm()&0022 != 0 {
		return errors.New("filesystem root must be root-owned and protected")
	}
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for i, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err = os.Mkdir(current, mode); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return errors.New("device guard directories and ancestors must be root-owned")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// macOS has root-owned /var and /tmp symlinks. A trusted link is safe only
			// when its entire resolved path has the same ancestor protection.
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return err
			}
			if err = protectedDirectoryPath(resolved, mode, i < len(components)-1 || allowStickyLeaf); err != nil {
				return err
			}
			current = resolved
			info, err = os.Lstat(current)
			if err != nil {
				return err
			}
		}
		stickyAncestor := (i < len(components)-1 || allowStickyLeaf) && info.Mode()&os.ModeSticky != 0
		if !info.IsDir() || info.Mode().Perm()&0022 != 0 && !stickyAncestor {
			return errors.New("device guard directory and ancestors must not be writable by other users")
		}
	}
	return nil
}

type unixControlListener struct{ *net.UnixListener }

func (l unixControlListener) Accept() (controlConn, error) {
	conn, err := l.AcceptUnix()
	if err != nil {
		return nil, err
	}
	return &unixControlConn{conn}, nil
}

type unixControlConn struct{ *net.UnixConn }

func (c *unixControlConn) Identity() (string, error) {
	uid, err := peerUID(c.UnixConn)
	return strconv.FormatUint(uint64(uid), 10), err
}
func (c *unixControlConn) Receive() (request, error) {
	data, err := readFrame(c.UnixConn)
	if err != nil {
		return request{}, err
	}
	var in request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&in); err != nil {
		return in, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return in, errors.New("invalid guard request")
	}
	return in, nil
}
func (c *unixControlConn) Send(out response, listener net.Listener) error {
	var rights []byte
	if listener != nil {
		file, err := listener.(*net.TCPListener).File()
		if err != nil {
			return err
		}
		defer file.Close()
		rights = unix.UnixRights(int(file.Fd()))
	}
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err = c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	n, _, err := c.WriteMsgUnix([]byte{0}, rights, nil)
	if err != nil {
		return err
	}
	if n != 1 {
		return io.ErrShortWrite
	}
	return writeFrame(c.UnixConn, data)
}
func listenControl(path string) (controlListener, error) {
	if err := protectedDirectory(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing non-socket guard path")
		}
		conn, e := net.DialTimeout("unix", path, 100*time.Millisecond)
		if e == nil {
			conn.Close()
			return nil, errors.New("device guard socket already active")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0666); err != nil {
		listener.Close()
		return nil, err
	}
	return unixControlListener{listener}, nil
}

func replaceStateFile(source, target string) error { return os.Rename(source, target) }
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

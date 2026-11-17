//go:build linux

package hostruntimecmd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// Fixed container layout: a deployment cannot select a socket, token or state path through its environment.
const (
	foregroundUID        = 10001
	foregroundState      = "/var/lib/paperboat"
	foregroundWorkspace  = "/workspace"
	foregroundHostdDir   = "/run/paperboat-hostd"
	foregroundUpdatedDir = "/run/paperboat-updated"
	foregroundExitConfig = 78 // sysexits EX_CONFIG: the environment or state is wrong, not the machine
	foregroundStopLimit  = 60 * time.Second
	foregroundPairLimit  = 5 * time.Minute
)

// NoSystemd reports a Linux machine that cannot run Paperboat as a service, typically a container. It is a
// variable so tests choose the answer.
var NoSystemd = func() bool {
	_, err := os.Stat("/run/systemd/system")
	return err != nil
}

// ForegroundHint is the one message every install and pair path prints on a machine without systemd.
func ForegroundHint(executable string) string {
	return "This machine has no systemd, so no service was installed or changed.\n" +
		"Run Paperboat as the container's main command (use an init: docker run --init):\n" +
		"  PAPERBOAT_ENROLLMENT_CREDENTIAL=<token from pb machine add> " + executable + " daemon --foreground\n" +
		"Dockerfile: CMD [\"" + executable + "\", \"daemon\", \"--foreground\"]. The token is only needed on the first start; the state volume keeps the enrollment."
}

type foregroundConfig struct {
	controlURL string
	token      string
	machineID  string
}

// foregroundFromEnv reads and validates every setting before any child starts. The control server defaults to
// the build's, like a normal install.
func foregroundFromEnv(environ func(string) string) (foregroundConfig, error) {
	controlURL := strings.TrimSpace(environ("PAPERBOAT_CONTROL_URL"))
	if controlURL == "" {
		controlURL = strings.TrimSpace(buildinfo.DefaultServerURL)
	}
	parsed, err := url.Parse(controlURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return foregroundConfig{}, errors.New("PAPERBOAT_CONTROL_URL must be an https URL (this build has no usable default)")
	}
	if workspace := environ("PAPERBOAT_WORKSPACE"); workspace != "" && workspace != foregroundWorkspace {
		return foregroundConfig{}, errors.New("PAPERBOAT_WORKSPACE must be " + foregroundWorkspace)
	}
	return foregroundConfig{controlURL: strings.TrimRight(controlURL, "/"), token: strings.TrimSpace(environ("PAPERBOAT_ENROLLMENT_CREDENTIAL")), machineID: environ("PAPERBOAT_MACHINE_ID")}, nil
}

// RunForeground is the container supervisor: it prepares the fixed state tree, pairs the machine on its first start
// from PAPERBOAT_ENROLLMENT_CREDENTIAL, then runs hostd as the runtime user until a stop signal or hostd's own exit.
// It never installs or changes a service. The return value is the process exit status.
func RunForeground(ctx context.Context, stdout, stderr io.Writer) int {
	// A stop signal during startup (pairing, the owner's start) ends it cleanly, not with the default kill that
	// would orphan a child already started.
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stopSignals()
	fail := func(err error) int {
		fmt.Fprintln(stderr, "paperboat container: "+err.Error())
		if ctx.Err() != nil {
			return 0 // a stop signal ended the start; the error above is what it interrupted
		}
		return foregroundExitConfig
	}
	config, err := foregroundFromEnv(os.Getenv)
	if err != nil {
		return fail(err)
	}
	if os.Geteuid() != 0 {
		return fail(errors.New("pb daemon --foreground must start as root: it runs the Paperboat service as an unprivileged runtime user"))
	}
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return fail(fmt.Errorf("resolving the pb executable failed: %w", err))
	}
	account, err := ensureRuntimeUser()
	if err != nil {
		return fail(err)
	}
	runtimeRoot, tokenFile := filepath.Join(foregroundState, "runtime"), filepath.Join(foregroundState, "hostd", "token")
	if err := prepareForegroundState(runtimeRoot, tokenFile); err != nil {
		return fail(err)
	}
	environ := foregroundEnvironment(os.Environ(), map[string]string{
		"PAPERBOAT_RUNTIME_STATE_ROOT": runtimeRoot, "PAPERBOAT_RUNTIME_CURRENT": executable,
		"PAPERBOAT_HOSTD_SOCKET": filepath.Join(foregroundHostdDir, "hostd.sock"), "PAPERBOAT_HOSTD_TOKEN_FILE": tokenFile,
		"PAPERBOAT_UPDATED_SOCKET": filepath.Join(foregroundUpdatedDir, "control.sock"),
		"PAPERBOAT_WORKSPACE_ROOT": foregroundWorkspace, "PAPERBOAT_WORKSPACE": foregroundWorkspace, "HOME": foregroundWorkspace,
		"PAPERBOAT_CONTROL_URL": config.controlURL,
	}, "PAPERBOAT_ENROLLMENT_CREDENTIAL", "PAPERBOAT_ENROLLMENT_CREDENTIAL_ENV", "NOTIFY_SOCKET")
	if _, err := enrollment.LoadRuntimeIdentityForRenewal(runtimeRoot, serverclock.Now().UTC()); err != nil {
		if err := pairForeground(ctx, executable, account, runtimeRoot, config, environ, stdout, stderr); err != nil {
			return fail(err)
		}
	}
	identity, err := enrollment.LoadRuntimeIdentityForRenewal(runtimeRoot, serverclock.Now().UTC())
	if err != nil {
		return fail(fmt.Errorf("reading the enrolled machine identity failed: %w", err))
	}
	if config.machineID != "" && config.machineID != identity.MachineID {
		return fail(errors.New("PAPERBOAT_MACHINE_ID is not the machine enrolled in this state volume; unset it or use a new volume"))
	}
	shell := foregroundShell()
	serveCtx, stopOwnerSocket := context.WithCancel(ctx)
	defer stopOwnerSocket()
	owners, catalogRoot, err := startForegroundOwners(ctx, serveCtx, executable, account, runtimeRoot, shell, filepath.Join(foregroundUpdatedDir, "control.sock"), stdout, stderr)
	if err != nil {
		return fail(err)
	}
	if ctx.Err() != nil {
		stopOwnerSocket()
		owners.stop(context.Background())
		return 0
	}
	environ = foregroundEnvironment(environ, map[string]string{"PAPERBOAT_MACHINE_ID": identity.MachineID, "PAPERBOAT_SESSION_OWNER_CATALOG": catalogRoot, "PAPERBOAT_SHELL": shell})
	status := superviseHostd(ctx, executable, account, environ, stdout, stderr)
	// The control socket closes first, then the owners stop and refuse new starts: no request can begin a child
	// that outlives the supervisor.
	stopOwnerSocket()
	owners.stop(context.Background())
	return status
}

// foregroundShell is the account's login shell, as an absolute path with no symlink.
func foregroundShell() string {
	shell := "/bin/sh"
	if _, err := os.Stat("/bin/bash"); err == nil {
		shell = "/bin/bash"
	}
	if resolved, err := filepath.EvalSymlinks(shell); err == nil {
		return resolved
	}
	return shell
}

// foregroundEnvironment returns base with the drop keys removed and set applied; it is KEY=value entries.
func foregroundEnvironment(base []string, set map[string]string, drop ...string) []string {
	skip := map[string]bool{}
	for _, key := range drop {
		skip[key] = true
	}
	for key := range set {
		skip[key] = true
	}
	result := make([]string, 0, len(base)+len(set))
	for _, entry := range base {
		if key, _, _ := strings.Cut(entry, "="); !skip[key] {
			result = append(result, entry)
		}
	}
	for key, value := range set {
		result = append(result, key+"="+value)
	}
	return result
}

// ensureRuntimeUser requires the runtime account. The image bakes it; a plain container gets it here, because
// hostd needs a real account for its shell and home. It writes only /etc/passwd and /etc/group, never a path that
// is read-only in the image.
func ensureRuntimeUser() (*user.User, error) {
	id := strconv.Itoa(foregroundUID)
	if account, err := user.LookupId(id); err == nil {
		return account, nil
	}
	if _, err := user.Lookup("paperboat"); err == nil {
		return nil, errors.New("the account name paperboat exists with another uid; remove it or give it uid " + id)
	}
	shell := foregroundShell()
	for path, line := range map[string]string{"/etc/group": "paperboat:x:" + id + ":\n", "/etc/passwd": "paperboat:x:" + id + ":" + id + ":Paperboat:" + foregroundWorkspace + ":" + shell + "\n"} {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return nil, fmt.Errorf("creating the paperboat runtime account failed (%s is not writable); build the image with a paperboat user of uid %s: %w", path, id, err)
		}
		_, writeErr := file.WriteString(line)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return nil, fmt.Errorf("creating the paperboat runtime account failed: %w", err)
		}
	}
	return user.LookupId(id)
}

// prepareForegroundState creates the fixed state tree with its owners and modes and the hostd capability token.
// Every path is checked after it is made: a symlink or another owner is refused, never followed.
func prepareForegroundState(runtimeRoot, tokenFile string) error {
	owner, root := [2]int{foregroundUID, foregroundUID}, [2]int{0, 0}
	for _, dir := range []struct {
		path  string
		mode  os.FileMode
		owner [2]int
	}{
		{foregroundState, 0o755, root}, {runtimeRoot, 0o700, owner}, {filepath.Dir(tokenFile), 0o700, owner},
		{foregroundWorkspace, 0o700, owner}, {foregroundHostdDir, 0o700, owner}, {foregroundUpdatedDir, 0o755, root},
	} {
		if err := ensureForegroundDir(dir.path, dir.mode, dir.owner[0], dir.owner[1]); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(tokenFile); errors.Is(err, os.ErrNotExist) {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return fmt.Errorf("creating the service capability token failed: %w", err)
		}
		file, err := os.OpenFile(tokenFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("creating the service capability token failed: %w", err)
		}
		_, writeErr := file.Write(secret)
		chownErr := file.Chown(foregroundUID, foregroundUID)
		if err := errors.Join(writeErr, chownErr, file.Close()); err != nil {
			return fmt.Errorf("creating the service capability token failed: %w", err)
		}
	}
	info, err := os.Lstat(tokenFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 32 || !ownedBy(info, foregroundUID, foregroundUID) {
		return errors.New("unsafe service capability token: remove " + tokenFile + " and restart to create a new one")
	}
	return nil
}

func ownedBy(info os.FileInfo, uid, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid && int(stat.Gid) == gid
}

func ensureForegroundDir(path string, mode os.FileMode, uid, gid int) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("unsafe path " + path + ": it must be a plain directory")
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("creating %s failed: %w", path, err)
		}
		if err := os.Mkdir(path, mode); err != nil {
			return fmt.Errorf("creating %s failed: %w", path, err)
		}
	default:
		return fmt.Errorf("checking %s failed: %w", path, err)
	}
	if err := errors.Join(os.Lchown(path, uid, gid), os.Chmod(path, mode)); err != nil {
		return fmt.Errorf("securing %s failed: %w", path, err)
	}
	if info, err := os.Lstat(path); err != nil || !info.IsDir() || info.Mode().Perm() != mode || !ownedBy(info, uid, gid) {
		return errors.New("unsafe path " + path + ": wrong owner or mode")
	}
	return nil
}

func runtimeCommand(ctx context.Context, executable string, account *user.User, environ []string, stdout, stderr io.Writer, args ...string) (*exec.Cmd, error) {
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if err := errors.Join(uidErr, gidErr); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Env, command.Stdout, command.Stderr = environ, stdout, stderr
	if uint32(os.Geteuid()) != uint32(uid) || uint32(os.Getegid()) != uint32(gid) {
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	}
	return command, nil
}

// pairForeground enrolls a machine whose state volume holds no identity yet, as the runtime user so every file it
// writes belongs to hostd. The one-shot token goes through an owner-only file, never a command line.
func pairForeground(ctx context.Context, executable string, account *user.User, runtimeRoot string, config foregroundConfig, environ []string, stdout, stderr io.Writer) error {
	if config.token == "" {
		return errors.New("this machine is not enrolled: set PAPERBOAT_ENROLLMENT_CREDENTIAL to a token from pb machine add for the first start")
	}
	tokenPath := filepath.Join(runtimeRoot, "enrollment-token")
	file, err := os.OpenFile(tokenPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("staging the enrollment token failed: %w", err)
	}
	_, writeErr := file.WriteString(config.token)
	chownErr := file.Chown(foregroundUID, foregroundUID)
	defer os.Remove(tokenPath)
	if err := errors.Join(writeErr, chownErr, file.Close()); err != nil {
		return fmt.Errorf("staging the enrollment token failed: %w", err)
	}
	pairCtx, cancel := context.WithTimeout(ctx, foregroundPairLimit)
	defer cancel()
	command, err := runtimeCommand(pairCtx, executable, account, environ, stdout, stderr,
		"daemon", "__runtime-bootstrap", "--server", config.controlURL, "--enrollment-token-file", tokenPath, "--state-root", runtimeRoot)
	if err != nil {
		return err
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("enrolling this machine failed (create a new token with pb machine add and restart): %w", err)
	}
	return nil
}

// superviseHostd runs hostd as the runtime user. A stop signal asks it to drain (SIGTERM, then SIGKILL after the
// limit) and exits 0; hostd ending by itself exits with its own status so the container restarts.
func superviseHostd(ctx context.Context, executable string, account *user.User, environ []string, stdout, stderr io.Writer) int {
	stop, cancel := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()
	command, err := runtimeCommand(context.Background(), executable, account, environ, stdout, stderr, "daemon", "__runtime-hostd")
	if err == nil {
		err = command.Start()
	}
	if err != nil {
		fmt.Fprintln(stderr, "paperboat container: starting the Paperboat service failed: "+err.Error())
		return foregroundExitConfig
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	select {
	case err := <-exited:
		return exitStatus(err)
	case <-stop.Done():
	}
	_ = command.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(foregroundStopLimit):
		_ = command.Process.Kill()
		<-exited
	}
	return 0
}

func exitStatus(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit) && exit.ExitCode() > 0:
		return exit.ExitCode()
	default:
		return 1
	}
}

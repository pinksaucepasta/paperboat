//go:build windows

package localdaemon

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sys/windows"
)

const windowsManagedAgentPipe = managedssh.WindowsInstalledAgentPipe

const (
	windowsManagedSSHMarker      = "paperboat-managed-ssh-v1"
	windowsManagedSSHBeginMarker = "# BEGIN paperboat-managed-ssh-v1"
	windowsManagedSSHEndMarker   = "# END paperboat-managed-ssh-v1"
	windowsManagedSSHMaxConfig   = 1 << 20
)

type ManagedSSHRuntime struct {
	closeFn func() error
	refresh func(context.Context, []api.UserMachine) error
	mu      sync.Mutex
	once    sync.Once
	err     error
}

func StartManagedSSH(ctx context.Context, cfg ManagedSSHConfig, machines []api.UserMachine) (*ManagedSSHRuntime, error) {
	if ctx == nil || strings.TrimSpace(cfg.ServerURL) == "" || cfg.Auth == nil || cfg.Store.Path == "" || strings.TrimSpace(cfg.CLIClientSessionID) == "" || !filepath.IsAbs(cfg.Home) || !filepath.IsAbs(cfg.RuntimeDirectory) || !filepath.IsAbs(cfg.Executable) {
		return nil, ErrInvalidInventoryConfig
	}
	identity, err := cfg.Store.ManagedSSHIdentity(cfg.ServerURL, cfg.CLIClientSessionID)
	if err != nil {
		return nil, err
	}
	credential, err := cfg.Auth.Credential()
	if err != nil {
		return nil, err
	}
	registerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	client := api.New(cfg.ServerURL, credential, nil)
	client.SetCredentialSource(cfg.Auth)
	_, err = client.RegisterManagedSSHClientKey(registerCtx, identity.PublicKey, identity.Fingerprint, "managed-ssh-register-"+hex.EncodeToString(identity.Fingerprint[:16]))
	cancel()
	if err != nil {
		return nil, managedSSHAuthorityError(err)
	}
	capabilities, err := managedssh.ProbeOpenSSH(ctx, "ssh.exe", 5*time.Second)
	if err != nil || !capabilities.Ready() {
		return nil, errors.Join(managedssh.ErrOpenSSHUnavailable, err)
	}
	agentRuntime, err := startWindowsAgent(ctx, identity.Signer, cfg.InheritedAgentSocket)
	if err != nil {
		return nil, err
	}
	install := func(refreshCtx context.Context, machines []api.UserMachine) error {
		// The alias domain comes from discovery and may change; it is one small request.
		configuration, err := client.ClientConfiguration(refreshCtx)
		if err != nil {
			return err
		}
		return installWindowsOpenSSHConfig(cfg, agentRuntime.socket, identity.PublicKey, configuration.LocalAccessBaseDomain, managedSSHAliasTargets(machines))
	}
	// ctx is the daemon's lifetime: the agent above serves on it. The first configuration install
	// asks the server, so it gets its own bound.
	installCtx, cancelInstall := context.WithTimeout(ctx, managedSSHInstallTimeout)
	err = install(installCtx, machines)
	cancelInstall()
	if err != nil {
		return nil, errors.Join(err, agentRuntime.Close())
	}
	return &ManagedSSHRuntime{closeFn: agentRuntime.Close, refresh: install}, nil
}

// Refresh rewrites the managed aliases for machines, the inventory's latest list.
func (r *ManagedSSHRuntime) Refresh(ctx context.Context, machines []api.UserMachine) error {
	if r == nil || r.refresh == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refresh(ctx, machines)
}

func (r *ManagedSSHRuntime) Close() error {
	if r == nil || r.closeFn == nil {
		return nil
	}
	r.once.Do(func() { r.err = r.closeFn() })
	return r.err
}

func ManagedSSHHealthCode(err error) string {
	return managedSSHHealthCode(err)
}

type windowsAgentRuntime struct {
	socket   string
	cancel   context.CancelFunc
	done     chan error
	delegate net.Conn
	once     sync.Once
	err      error
}

func startWindowsAgent(parent context.Context, signer ssh.Signer, inherited string) (*windowsAgentRuntime, error) {
	if parent == nil || signer == nil {
		return nil, managedssh.ErrAgentDenied
	}
	ownerSID, err := currentWindowsUserSID()
	if err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(windowsManagedAgentPipe, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + ownerSID + ")",
		MessageMode:        false,
		InputBufferSize:    managedssh.MaxAgentRequestBytes + 4,
		OutputBufferSize:   managedssh.MaxAgentRequestBytes + 4,
	})
	if err != nil {
		return nil, err
	}
	managed, err := managedssh.NewAgent(signer)
	if err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	var delegate agent.ExtendedAgent
	var delegateConnection net.Conn
	if inherited != "" {
		if !validWindowsAgentPipe(inherited) || strings.EqualFold(inherited, windowsManagedAgentPipe) {
			return nil, errors.Join(managedssh.ErrAgentDenied, listener.Close())
		}
		delegateConnection, err = winio.DialPipeContext(parent, inherited)
		if err != nil {
			return nil, errors.Join(err, listener.Close())
		}
		delegate = agent.NewClient(delegateConnection)
	}
	aggregate, err := managedssh.NewAggregate(managed, delegate)
	if err != nil {
		cleanupErr := listener.Close()
		if delegateConnection != nil {
			cleanupErr = errors.Join(cleanupErr, delegateConnection.Close())
		}
		return nil, errors.Join(err, cleanupErr)
	}
	runCtx, cancel := context.WithCancel(parent)
	runtime := &windowsAgentRuntime{socket: windowsManagedAgentPipe, cancel: cancel, done: make(chan error, 1), delegate: delegateConnection}
	go func() {
		runtime.done <- (managedssh.Server{Agent: aggregate, MaxConnections: 32, IdleTimeout: 2 * time.Minute}).Serve(runCtx, listener)
	}()
	return runtime, nil
}

func (r *windowsAgentRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		r.cancel()
		r.err = <-r.done
		if r.delegate != nil {
			r.err = errors.Join(r.err, r.delegate.Close())
		}
	})
	return r.err
}

func validWindowsAgentPipe(value string) bool {
	const prefix = "\\\\.\\pipe\\"
	return strings.HasPrefix(strings.ToLower(value), prefix) && len(value) > len(prefix) && len(value) <= 256 && !strings.ContainsAny(value[len(prefix):], "/\\\\:*?\"<>|"+"\x00\r\n")
}

func installWindowsOpenSSHConfig(cfg ManagedSSHConfig, agentSocket, publicKey, domain string, targets []managedssh.OpenSSHAliasTarget) error {
	if err := managedssh.InstallManagedIdentityPublicKey(cfg.Home, cfg.OwnerUID, publicKey); err != nil {
		return err
	}
	identityFile := managedssh.ManagedIdentityPublicKeyPath(cfg.Home)
	executable := quoteWindowsOpenSSH(cfg.Executable)
	_, err := managedssh.InstallOpenSSHConfig(managedssh.OpenSSHConfig{
		Home: cfg.Home, OwnerUID: cfg.OwnerUID, AliasDomain: domain,
		ProxyCommand:      managedSSHProxyCommand(executable),
		KnownHostsCommand: executable + " __ssh-known-hosts --host %h --port %p",
		AgentSocket:       agentSocket,
		IdentityFile:      identityFile,
		Targets:           targets,
	})
	return err
}

func ensureWindowsSSHDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidInventoryConfig
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return managedssh.ErrOpenSSHConfigConflict
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return managedssh.ErrOpenSSHConfigConflict
	}
	return nil
}

func readWindowsSSHConfig(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", managedssh.ErrOpenSSHConfigConflict
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.Size() > windowsManagedSSHMaxConfig {
		return "", managedssh.ErrOpenSSHConfigConflict
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) > windowsManagedSSHMaxConfig || strings.ContainsRune(string(body), 0) {
		return "", managedssh.ErrOpenSSHConfigConflict
	}
	return string(body), nil
}

func quoteWindowsOpenSSH(value string) string {
	return "\"" + strings.ReplaceAll(value, "\"", "\\\"") + "\""
}

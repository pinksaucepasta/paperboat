package windowsopenssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type SetupResult struct {
	Result
	CreatedHostKey bool
}

// PrepareSharedState verifies or provisions the approved OpenSSH payload and
// prepares its managed configuration and persistent keys without registering or
// starting a service. Generation wrappers own their own activation and health.
func PrepareSharedState(ctx context.Context, config Config) (SetupResult, error) {
	if config.ServiceName == "" {
		config.ServiceName = ServiceName
	}
	result, err := prepareSharedState(ctx, config)
	if err != nil {
		return SetupResult{}, err
	}
	if err := RepairFirewallOwnership(ctx, config); err != nil {
		return SetupResult{}, err
	}
	return result, nil
}

func prepareSharedState(ctx context.Context, config Config) (SetupResult, error) {
	if config.ServiceName == "" {
		config.ServiceName = ServiceName
	}
	inventory, err := Inventory(ctx, config)
	if err != nil {
		return SetupResult{}, err
	}
	var result Result
	if inventory.Class == InstallationPaperboatApproved {
		result, err = verifyInstalledResult(ctx, config)
	} else {
		result, err = Provision(ctx, config)
	}
	if err != nil {
		return SetupResult{}, err
	}
	return prepareStateFromResult(ctx, config, result)
}

func Setup(ctx context.Context, config Config) (SetupResult, error) {
	if config.ServiceName == "" {
		config.ServiceName = ServiceName
	}
	prepared, err := prepareSharedState(ctx, config)
	if err != nil {
		return SetupResult{}, err
	}
	return activatePreparedState(ctx, config, prepared)
}

func setupFromResult(ctx context.Context, config Config, result Result) (SetupResult, error) {
	prepared, err := prepareStateFromResult(ctx, config, result)
	if err != nil {
		return SetupResult{}, err
	}
	return activatePreparedState(ctx, config, prepared)
}

func prepareStateFromResult(ctx context.Context, config Config, result Result) (SetupResult, error) {
	// Fail closed before writing anything: without the owner there is no account
	// to scope Paperboat's key to, and no name or global fallback.
	managedUser, err := managedUserForSID(config.OwnerSID)
	if err != nil {
		return SetupResult{}, err
	}
	if err := protectManagedKeys(ctx, config.StateRoot); err != nil {
		return SetupResult{}, err
	}
	configPath, err := WriteServiceConfig(ServiceConfig{
		StateRoot: config.StateRoot, SSHDPath: result.SSHDPath, SFTPPath: result.SFTPPath,
		AuthorizedKeys: filepath.Join(config.StateRoot, "authorized_keys", "paperboat"),
		ManagedUser:    managedUser, OwnerSID: config.OwnerSID, Port: result.Port,
	})
	if err != nil {
		return SetupResult{}, err
	}
	created, err := EnsureHostKey(ctx, config)
	if err != nil {
		return SetupResult{}, err
	}
	if err := ValidateServiceConfig(config.Runner, result.SSHDPath, configPath); err != nil {
		return SetupResult{}, err
	}
	result.ConfigPath = configPath
	return SetupResult{Result: result, CreatedHostKey: created}, nil
}

// EnsureHostKey creates the managed sshd host key when its file is missing and protects the key files. Install and
// repair both call it: repair of an installation whose service still exists used to skip it, so deleted key files made
// the service unhealthy and `pb doctor --repair` fail.
func EnsureHostKey(ctx context.Context, config Config) (created bool, err error) {
	hostKey := filepath.Join(config.StateRoot, "hostkeys", "ssh_host_ed25519_key")
	if _, err := os.Lstat(hostKey); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(hostKey), 0o700); err != nil {
			return false, operationFailure("Windows could not create the managed SSH host-key directory", ErrInstallFailed, err)
		}
		keygen := filepath.Join(config.InstallRoot, "ssh-keygen.exe")
		if err := verifyBinary(ctx, config, keygen); err != nil {
			return false, err
		}
		keyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, keyErr := config.Runner.Run(keyCtx, keygen, "-q", "-t", "ed25519", "-N", "", "-f", hostKey)
		cancel()
		if keyErr != nil {
			return false, operationFailure("Windows could not create the managed SSH host key; retry setup after checking the state directory permissions", ErrInstallFailed, keyErr)
		}
		created = true
	} else if err != nil {
		return false, err
	}
	if err := protectHostKeyFiles(hostKey); err != nil {
		return false, err
	}
	return created, protectHostPublicKeyFile(hostKey+".pub", config.OwnerSID, config.ServiceSID)
}

func activatePreparedState(ctx context.Context, config Config, prepared SetupResult) (SetupResult, error) {
	result := prepared.Result
	serviceExecutable, err := paperboatServiceExecutable(config.ServiceExecutable)
	if err != nil {
		return SetupResult{}, err
	}
	if err := InstallService(ctx, config.ServiceName, serviceExecutable, result.SSHDPath, result.ConfigPath, config.OwnerSID); err != nil {
		return SetupResult{}, err
	}
	healthCtx, cancelHealth := context.WithTimeout(ctx, 15*time.Second)
	defer cancelHealth()
	if _, err := CheckLoopbackHealth(healthCtx, config, result); err != nil {
		return SetupResult{}, err
	}
	if err := RepairFirewallOwnership(ctx, config); err != nil {
		return SetupResult{}, err
	}
	return prepared, nil
}

func paperboatServiceExecutable(configured string) (string, error) {
	path := configured
	if path == "" {
		path = os.Getenv("PAPERBOAT_SERVICE_EXECUTABLE")
	}
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.Join(ErrServiceOwnership, err)
	}
	return filepath.Clean(path), nil
}

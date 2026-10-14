package windowsopenssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
)

const (
	MinimumWingetVersion = "1.7.0"
	ServiceName          = "PaperboatSshd"
	securityModuleImport = "$m=Join-Path $env:WINDIR 'System32\\WindowsPowerShell\\v1.0\\Modules\\Microsoft.PowerShell.Security\\Microsoft.PowerShell.Security.psd1';Import-Module -Name $m -ErrorAction Stop;"
)

var (
	ErrInvalidConfig        = errors.New("invalid Windows OpenSSH configuration")
	ErrInstallerUnavailable = errors.New("openssh_installer_unavailable")
	ErrInstallFailed        = errors.New("openssh_install_failed")
	ErrUntrustedBinary      = errors.New("openssh_binary_untrusted")
	ErrVersionMismatch      = errors.New("openssh_version_mismatch")
	ErrConflictingInstall   = errors.New("openssh_conflicting_installation")
	ErrRepairFailed         = errors.New("openssh_repair_failed")
	ErrServiceUnhealthy     = errors.New("openssh_service_unhealthy")
	ErrServiceOwnership     = errors.New("openssh_service_ownership_conflict")
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	processlaunch.ConfigureBackground(command)
	// Keep stdout and stderr on distinct OS handles. Windows OpenSSH and
	// PowerShell attach stream semantics to those handles; merging them at the
	// process boundary can make an otherwise successful remote command report a
	// failed exit status. We merge only after the child has exited.
	var stdout, stderr bytes.Buffer
	// Supply a real, immediately-closed stdin pipe. A nil standard handle makes
	// the Win32 OpenSSH client leave output-producing exec channels open until
	// our deadline even after the server has sent exit-status.
	command.Stdin = bytes.NewReader(nil)
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	output := append([]byte(nil), stdout.Bytes()...)
	output = append(output, stderr.Bytes()...)
	return output, err
}

type Config struct {
	Platform          string
	Architecture      string
	WingetPath        string
	InstallRoot       string
	StateRoot         string
	ApprovedVersion   string
	ExpectedPublisher string
	OwnerSID          string
	ServiceSID        string
	ServiceExecutable string
	ServiceName       string
	Port              uint16
	Runner            Runner
}

type Result struct {
	PackageID      string
	Version        string
	SSHPath        string
	SSHDPath       string
	SCPPath        string
	SFTPClientPath string
	SFTPPath       string
	KeygenPath     string
	ConfigPath     string
	Port           uint16
}

type windowsOperationError struct {
	message        string
	classification error
	cause          error
}

func (e *windowsOperationError) Error() string {
	if e == nil || e.message == "" {
		return "Windows OpenSSH operation failed"
	}
	return e.message
}

func (e *windowsOperationError) Unwrap() []error {
	if e == nil {
		return nil
	}
	causes := make([]error, 0, 2)
	if e.classification != nil {
		causes = append(causes, e.classification)
	}
	if e.cause != nil {
		causes = append(causes, e.cause)
	}
	return causes
}

func operationFailure(message string, classification, cause error) error {
	return &windowsOperationError{message: message, classification: classification, cause: cause}
}

func DefaultConfig(runner Runner) Config {
	if runner == nil {
		runner = CommandRunner{}
	}
	programFiles := os.Getenv("ProgramFiles")
	programData := os.Getenv("ProgramData")
	// Elevated Windows processes normally inherit these variables, but service
	// repair can be launched from a restricted environment. The documented
	// machine-wide locations remain authoritative in that case.
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return Config{
		Platform: runtime.GOOS, Architecture: runtime.GOARCH,
		InstallRoot: filepath.Join(programFiles, "OpenSSH"),
		StateRoot:   filepath.Join(programData, "Paperboat", "ssh"), ApprovedVersion: ApprovedVersion,
		ExpectedPublisher: compatibility.ExpectedPublisher, OwnerSID: platformOwnerSID(), ServiceSID: platformServiceSID(), ServiceName: ServiceName, Port: 38222, Runner: runner,
	}
}

func Provision(ctx context.Context, config Config) (Result, error) {
	return provision(ctx, config, false)
}

func provision(ctx context.Context, config Config, force bool) (Result, error) {
	if err := validate(config); err != nil {
		return Result{}, err
	}
	if config.Platform != "windows" {
		return Result{}, ErrInstallerUnavailable
	}
	beforeFirewall, firewallErr := snapshotFirewall(ctx, config)
	if firewallErr != nil {
		return Result{}, firewallErr
	}
	wingetPath := config.WingetPath
	useSystemModule := false
	if wingetPath == "" {
		var err error
		wingetPath, err = resolveWinget(ctx, config.Runner)
		if err != nil {
			if moduleErr := ensureSystemWingetModule(ctx, config.Runner); moduleErr != nil {
				return Result{}, errors.Join(err, moduleErr)
			}
			useSystemModule = true
		}
	}
	if !useSystemModule {
		verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		versionOutput, err := config.Runner.Run(verifyCtx, wingetPath, "--version")
		cancel()
		if err != nil || !supportedWingetVersion(string(versionOutput)) {
			versionFailure := operationFailure("could not verify the approved Windows package-management client", ErrInstallerUnavailable, err)
			if err == nil {
				versionFailure = operationFailure("installed Windows package-management client is not supported", ErrInstallerUnavailable, nil)
			}
			if config.WingetPath != "" {
				return Result{}, errors.Join(ErrInstallerUnavailable, versionFailure)
			}
			if moduleErr := ensureSystemWingetModule(ctx, config.Runner); moduleErr != nil {
				return Result{}, errors.Join(ErrInstallerUnavailable, versionFailure, moduleErr)
			}
			useSystemModule = true
		}
	}
	if useSystemModule {
		if err := installWithSystemWinget(ctx, config, force); err != nil {
			return Result{}, err
		}
	} else {
		installCtx, cancelInstall := context.WithTimeout(ctx, 10*time.Minute)
		installArgs := []string{
			"install", "--exact", "--id", PackageID, "--version", config.ApprovedVersion,
			"--source", "winget", "--scope", "machine", "--silent",
			"--accept-source-agreements", "--accept-package-agreements", "--disable-interactivity",
		}
		if force {
			installArgs = append(installArgs, "--force")
		}
		_, err := config.Runner.Run(installCtx, wingetPath, installArgs...)
		cancelInstall()
		if err != nil {
			return Result{}, operationFailure("Windows could not install the approved OpenSSH package; check WinGet access and administrator approval, then retry setup", ErrInstallFailed, err)
		}
	}
	afterFirewall, firewallErr := snapshotFirewall(ctx, config)
	if firewallErr != nil {
		return Result{}, firewallErr
	}
	result := resultForConfig(config)
	for _, path := range []string{result.SSHPath, result.SSHDPath, result.SCPPath, result.SFTPClientPath, result.SFTPPath, result.KeygenPath} {
		if err := verifyBinary(ctx, config, path); err != nil {
			return Result{}, err
		}
	}
	if err := persistFirewallOwnership(ctx, config, beforeFirewall, afterFirewall); err != nil {
		return Result{}, err
	}
	return result, nil
}

func resultForConfig(config Config) Result {
	return Result{
		PackageID:      PackageID,
		Version:        config.ApprovedVersion,
		SSHPath:        filepath.Join(config.InstallRoot, "ssh.exe"),
		SSHDPath:       filepath.Join(config.InstallRoot, "sshd.exe"),
		SCPPath:        filepath.Join(config.InstallRoot, "scp.exe"),
		SFTPClientPath: filepath.Join(config.InstallRoot, "sftp.exe"),
		SFTPPath:       filepath.Join(config.InstallRoot, "sftp-server.exe"),
		KeygenPath:     filepath.Join(config.InstallRoot, "ssh-keygen.exe"),
		ConfigPath:     filepath.Join(config.StateRoot, "sshd_config"),
		Port:           config.Port,
	}
}

func verifyInstalledResult(ctx context.Context, config Config) (Result, error) {
	result := resultForConfig(config)
	for _, path := range []string{result.SSHPath, result.SSHDPath, result.SCPPath, result.SFTPClientPath, result.SFTPPath, result.KeygenPath} {
		if err := verifyBinary(ctx, config, path); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

func verifyBinary(ctx context.Context, config Config, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return operationFailure("installed OpenSSH executable could not be inspected", ErrUntrustedBinary, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return operationFailure("installed OpenSSH executable is not a regular file", ErrUntrustedBinary, nil)
	}
	escaped := strings.ReplaceAll(path, "'", "''")
	expectedPublisher := strings.ReplaceAll(config.ExpectedPublisher, "'", "''")
	expectedVersion := strings.ReplaceAll(config.ApprovedVersion, "'", "''")
	expectedMachine := "0"
	if config.Architecture == "amd64" {
		expectedMachine = "0x8664"
	} else if config.Architecture == "arm64" {
		expectedMachine = "0xaa64"
	}
	script := securityModuleImport + "$p='" + escaped + "';$s=Get-AuthenticodeSignature -LiteralPath $p;" +
		"$v=(Get-Item -LiteralPath $p).VersionInfo.FileVersion;" +
		"if($s.Status -ne 'Valid' -or $s.SignerCertificate.Subject -notlike '*" + expectedPublisher + "*'){exit 41};" +
		"if($v -notlike '" + expectedVersion + "*'){exit 42};" +
		"if(" + expectedMachine + "-ne 0){$f=[IO.File]::OpenRead($p);try{$r=[IO.BinaryReader]::new($f);$f.Position=0x3c;$o=$r.ReadUInt32();$f.Position=$o+4;$m=$r.ReadUInt16();if($m-ne" + expectedMachine + "){exit 43}}finally{$f.Dispose()}}"
	_, err = config.Runner.Run(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			cause := context.Cause(ctx)
			if cause == nil {
				cause = ctx.Err()
			}
			return operationFailure("Windows OpenSSH signature verification was interrupted", nil, errors.Join(cause, err))
		}
		if qualificationExitCode(err) == 42 {
			return operationFailure("installed OpenSSH version does not match the approved package", ErrVersionMismatch, err)
		}
		return operationFailure("Windows OpenSSH signature, publisher, version, or architecture verification failed", ErrUntrustedBinary, err)
	}
	return nil
}

func validate(config Config) error {
	if config.Platform != "windows" || config.Runner == nil || config.WingetPath != "" && strings.TrimSpace(config.WingetPath) == "" || !filepath.IsAbs(config.InstallRoot) ||
		!filepath.IsAbs(config.StateRoot) || config.ApprovedVersion == "" || config.ExpectedPublisher == "" || config.Port == 0 || config.Architecture != "" && !slices.Contains(compatibility.Architectures, config.Architecture) {
		return ErrInvalidConfig
	}
	return nil
}

func resolveWinget(ctx context.Context, runner Runner) (string, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	script := securityModuleImport + "$p=Get-AppxPackage -Name Microsoft.DesktopAppInstaller -ErrorAction Stop|Sort-Object Version -Descending|Select-Object -First 1;" +
		"$w=Join-Path $p.InstallLocation 'winget.exe';$s=Get-AuthenticodeSignature -LiteralPath $w;" +
		"if($s.Status -ne 'Valid' -or $s.SignerCertificate.Subject -notlike '*Microsoft*'){exit 41};Write-Output $w"
	output, err := runner.Run(resolveCtx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	path := strings.TrimSpace(string(output))
	if err != nil {
		return "", operationFailure("could not locate the approved Windows package-management client", ErrInstallerUnavailable, err)
	}
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Base(path), "winget.exe") {
		return "", operationFailure("could not locate the approved Windows package-management client", ErrInstallerUnavailable, nil)
	}
	return filepath.Clean(path), nil
}

func supportedWingetVersion(value string) bool {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	var major, minor int
	if _, err := fmt.Sscanf(value, "%d.%d", &major, &minor); err != nil {
		return false
	}
	return major > 1 || major == 1 && minor >= 7
}

func boundedOutput(value []byte) string {
	value = bytes.TrimSpace(value)
	if len(value) > 2048 {
		value = value[:2048]
	}
	return string(value)
}

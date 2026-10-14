package windowsopenssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type verificationOutputRunner struct {
	output []byte
	err    error
}

func (r verificationOutputRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return append([]byte(nil), r.output...), r.err
}

type runnerReply struct {
	output []byte
	err    error
}

type queuedRunner struct {
	replies []runnerReply
	index   int
}

func (r *queuedRunner) Run(context.Context, string, ...string) ([]byte, error) {
	if r.index >= len(r.replies) {
		return nil, errors.New("unexpected extra runner call")
	}
	reply := r.replies[r.index]
	r.index++
	return append([]byte(nil), reply.output...), reply.err
}

func TestVerifyBinaryClassifiesVersionExitAndKeepsCauseWithoutTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sshd.exe")
	if err := os.WriteFile(path, []byte("test executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	commandErr := commandExitFailure(t, 42)
	const privateTranscript = "private Windows path C:\\Users\\Alice\\OpenSSH\\sshd.exe"
	err := verifyBinary(context.Background(), Config{
		Runner: verificationOutputRunner{output: []byte(privateTranscript), err: commandErr},
	}, path)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("verification error = %v, want version mismatch", err)
	}
	if !errors.Is(err, commandErr) {
		t.Fatalf("verification error %v lost process exit cause %v", err, commandErr)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("verification error exposed command output: %v", err)
	}
}

func TestVerifyBinaryKeepsInspectionCauseWithoutExposingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "sshd.exe")
	err := verifyBinary(context.Background(), Config{}, path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification error = %v, want filesystem cause", err)
	}
	if strings.Contains(err.Error(), path) {
		t.Fatalf("verification error exposed private executable path: %v", err)
	}
}

func TestVerifyBinaryPreservesCancellationCauseAndOperationalFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sshd.exe")
	if err := os.WriteFile(path, []byte("test executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	parentCause := errors.New("verification interrupted by an I/O failure")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(parentCause)
	commandCause := errors.New("signature process failed")
	err := verifyBinary(ctx, Config{Runner: verificationOutputRunner{
		output: []byte("private diagnostic"), err: commandCause,
	}}, path)
	if !errors.Is(err, parentCause) || !errors.Is(err, commandCause) {
		t.Fatalf("verification error = %v, want both cancellation cause and process failure", err)
	}
	if errors.Is(err, ErrUntrustedBinary) {
		t.Fatalf("canceled verification was classified as an untrusted binary: %v", err)
	}
	if strings.Contains(err.Error(), "private diagnostic") {
		t.Fatalf("verification error exposed command output: %v", err)
	}
}

func TestProvisionKeepsInstallCauseWithoutTranscript(t *testing.T) {
	const privateTranscript = "private package source C:\\Users\\Alice\\AppData\\Local\\Temp"
	cause := errors.New("package manager failed")
	replies := []runnerReply{
		{output: []byte("v1.7.0")},
		{output: []byte(privateTranscript), err: cause},
	}
	if runtime.GOOS == "windows" {
		replies = append([]runnerReply{{output: []byte(`{"captured_at":"2026-01-01T00:00:00Z","system_sshd":false,"profiles":[],"openssh_inbound":[]}`)}}, replies...)
	}
	runner := &queuedRunner{replies: replies}
	root := t.TempDir()
	_, err := Provision(context.Background(), Config{
		Platform: "windows", WingetPath: "winget.exe", InstallRoot: filepath.Join(root, "OpenSSH"),
		StateRoot: filepath.Join(root, "state"), ApprovedVersion: ApprovedVersion,
		ExpectedPublisher: "Microsoft", Port: 38222, Runner: runner,
	})
	if !errors.Is(err, ErrInstallFailed) || !errors.Is(err, cause) {
		t.Fatalf("provision error = %v, want install classification and runner cause", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("provision error exposed command output: %v", err)
	}
}

func TestSystemWingetInstallKeepsCauseWithoutTranscript(t *testing.T) {
	const privateTranscript = "private package source C:\\Users\\Alice\\AppData\\Local\\Temp"
	cause := errors.New("system package install failed")
	runner := &queuedRunner{replies: []runnerReply{
		{output: []byte(approvedWinGetModuleVersion)},
		{output: []byte(privateTranscript), err: cause},
	}}
	err := installWithSystemWinget(context.Background(), Config{Runner: runner, ApprovedVersion: ApprovedVersion}, false)
	if !errors.Is(err, ErrInstallFailed) || !errors.Is(err, cause) {
		t.Fatalf("system WinGet error = %v, want install classification and runner cause", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("system WinGet error exposed command output: %v", err)
	}
}

func TestSystemWingetPreparationKeepsCauseWithoutTranscript(t *testing.T) {
	const privateTranscript = "private module path C:\\Users\\Alice\\Documents\\PowerShell"
	cause := errors.New("package-management setup failed")
	err := ensureSystemWingetModule(context.Background(), verificationOutputRunner{
		output: []byte(privateTranscript), err: cause,
	})
	if !errors.Is(err, ErrInstallerUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("package-management preparation error = %v, want installer classification and runner cause", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("package-management preparation error exposed command output: %v", err)
	}
}

func TestSystemWingetPreparationDoesNotExposeUnexpectedVersionOutput(t *testing.T) {
	const privateTranscript = "unexpected private module output C:\\Users\\Alice\\Documents\\PowerShell"
	err := ensureSystemWingetModule(context.Background(), verificationOutputRunner{output: []byte(privateTranscript)})
	if !errors.Is(err, ErrInstallerUnavailable) {
		t.Fatalf("package-management preparation error = %v, want installer unavailable", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("package-management preparation error exposed command output: %v", err)
	}
}

func TestSetupHostKeyGenerationKeepsCauseWithoutTranscript(t *testing.T) {
	const privateTranscript = "private host key C:\\Users\\Alice\\ProgramData\\Paperboat\\ssh"
	cause := errors.New("host-key generator failed")
	root := t.TempDir()
	install := filepath.Join(root, "OpenSSH")
	if err := os.MkdirAll(install, 0o700); err != nil {
		t.Fatal(err)
	}
	keygen := filepath.Join(install, "ssh-keygen.exe")
	if err := os.WriteFile(keygen, []byte("test executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &queuedRunner{replies: []runnerReply{
		{},
		{output: []byte(privateTranscript), err: cause},
	}}
	config := Config{InstallRoot: install, StateRoot: filepath.Join(root, "state"), Port: 38222, Runner: runner}
	_, err := setupFromResult(context.Background(), config, resultForConfig(config))
	if !errors.Is(err, ErrInstallFailed) || !errors.Is(err, cause) {
		t.Fatalf("setup error = %v, want install classification and runner cause", err)
	}
	if strings.Contains(err.Error(), privateTranscript) {
		t.Fatalf("setup error exposed command output: %v", err)
	}
}

func commandExitFailure(t *testing.T, code int) error {
	t.Helper()
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/c", "exit", strconv.Itoa(code))
	} else {
		command = exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	}
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		t.Fatalf("exit command error = %v, want process exit %d", err, code)
	}
	return err
}

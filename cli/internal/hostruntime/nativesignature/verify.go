// Package nativesignature verifies operating-system signature integrity. TUF
// establishes release identity; publisher certificates and notarization are
// optional. Darwin executable code signatures must remain valid.
package nativesignature

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
)

var ErrInvalid = errors.New("native executable signature verification failed")

// Runner is deliberately small so verification behavior can be tested without
// a macOS code-signing identity or a Windows certificate store.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// CommandRunner invokes a native verification utility without a shell.
type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	processlaunch.ConfigureBackground(command)
	return command.CombinedOutput()
}

// Verifier performs the native checks for a staged target. Linux targets have
// no additional platform signing format in this release; their TUF digest and
// ELF validation remain mandatory. Callers also validate each executable format.
type Verifier struct {
	Runner Runner
}

func New(runner Runner) Verifier {
	if runner == nil {
		runner = CommandRunner{}
	}
	return Verifier{Runner: runner}
}

func (v Verifier) Verify(ctx context.Context, path, platform, architecture string) error {
	if v.Runner == nil || path == "" || !validArchitecture(architecture) {
		return ErrInvalid
	}
	switch platform {
	case "linux":
		return nil
	case "darwin":
		return v.verifyDarwin(ctx, path)
	case "windows":
		// Authenticode is intentionally optional. Release integrity comes from
		// the TUF target digest and PE machine validation performed by callers.
		return nil
	default:
		return ErrInvalid
	}
}

func (v Verifier) verifyDarwin(ctx context.Context, path string) error {
	if strings.EqualFold(filepath.Ext(path), ".pkg") {
		output, err := v.Runner.Run(ctx, "/usr/sbin/pkgutil", "--check-signature", path)
		if err != nil && !isUnsignedPackage(output) {
			return fmt.Errorf("%w: package signature", ErrInvalid)
		}
		// TUF authenticates unsigned packages. When a publisher signature is
		// present, pkgutil must validate it. Gatekeeper admission additionally
		// requires optional publisher/notarization credentials, so it is not an
		// updater release-identity check.
		return nil
	}
	// Validate the embedded code signature, including the ad-hoc signature
	// produced by the package builder. TUF establishes publisher identity;
	// Gatekeeper admission is not required by the release trust policy.
	if _, err := v.Runner.Run(ctx, "codesign", "--verify", "--deep", "--strict", "--verbose=2", path); err != nil {
		return fmt.Errorf("%w: codesign", ErrInvalid)
	}
	return nil
}

func isUnsignedPackage(output []byte) bool {
	value := strings.ToLower(string(output))
	return strings.Contains(value, "no signature") || strings.Contains(value, "not signed")
}

func validArchitecture(architecture string) bool {
	return architecture == "amd64" || architecture == "arm64"
}

// Package nativesignature verifies operating-system signature integrity. TUF
// establishes release identity; publisher certificates and notarization are
// optional. Darwin executable code signatures must remain valid. A Windows
// release is Authenticode-checked only when its signed release metadata marks
// the target as signed (releaseindex.Target.AuthenticodeSigned); until the
// release workflow signs and marks it, the TUF digest and PE checks are the
// trust root.
package nativesignature

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
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

// VerifyRelease is Verify plus the Windows publisher check the signed release metadata asks for.
func (v Verifier) VerifyRelease(ctx context.Context, path, platform, architecture string, authenticodeSigned bool) error {
	if err := v.Verify(ctx, path, platform, architecture); err != nil {
		return err
	}
	if platform == "windows" && authenticodeSigned {
		return v.verifyAuthenticode(ctx, path, buildinfo.WindowsPublisher)
	}
	return nil
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
		// Authenticode is required only through VerifyRelease, when the signed metadata marks the target.
		// Otherwise release integrity comes from the TUF target digest and PE machine validation performed by callers.
		return nil
	default:
		return ErrInvalid
	}
}

// verifyAuthenticode requires a valid, chained Authenticode signature whose signer certificate's simple name
// (its CN) is exactly publisher. The path travels base64-encoded so no file name can alter the script.
func (v Verifier) verifyAuthenticode(ctx context.Context, path, publisher string) error {
	if publisher == "" {
		return fmt.Errorf("%w: no Windows publisher is configured", ErrInvalid)
	}
	script := "$ErrorActionPreference='Stop';" +
		"Import-Module -Name (Join-Path $env:WINDIR 'System32\\WindowsPowerShell\\v1.0\\Modules\\Microsoft.PowerShell.Security\\Microsoft.PowerShell.Security.psd1');" +
		"$p=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + base64.StdEncoding.EncodeToString([]byte(path)) + "'));" +
		"$s=Get-AuthenticodeSignature -LiteralPath $p;$n='';if($null -ne $s.SignerCertificate){$n=$s.SignerCertificate.GetNameInfo('SimpleName',$false)};" +
		"[pscustomobject]@{status=[string]$s.Status;publisher=[string]$n}|ConvertTo-Json -Compress"
	output, err := v.Runner.Run(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	if err != nil {
		return fmt.Errorf("%w: Authenticode query", ErrInvalid)
	}
	var result struct {
		Status    string `json:"status"`
		Publisher string `json:"publisher"`
	}
	if json.Unmarshal(output, &result) != nil || result.Status != "Valid" {
		return fmt.Errorf("%w: Authenticode signature is %q", ErrInvalid, result.Status)
	}
	if result.Publisher != publisher {
		return fmt.Errorf("%w: Authenticode publisher is not %s", ErrInvalid, publisher)
	}
	return nil
}

func (v Verifier) verifyDarwin(ctx context.Context, path string) error {
	// Validate the embedded code signature, including the ad-hoc signature
	// produced by the release runner. TUF establishes publisher identity;
	// Gatekeeper admission is not required by the release trust policy.
	if _, err := v.Runner.Run(ctx, "codesign", "--verify", "--deep", "--strict", "--verbose=2", path); err != nil {
		return fmt.Errorf("%w: codesign", ErrInvalid)
	}
	return nil
}

func validArchitecture(architecture string) bool {
	return architecture == "amd64" || architecture == "arm64"
}

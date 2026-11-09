package nativesignature

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
)

type call struct {
	name string
	args []string
}

type fakeRunner struct {
	calls            []call
	output           []byte
	err              error
	rejectGatekeeper bool
}

func (r *fakeRunner) Run(_ context.Context, name string, arguments ...string) ([]byte, error) {
	r.calls = append(r.calls, call{name: name, args: append([]string(nil), arguments...)})
	if r.rejectGatekeeper && strings.HasSuffix(name, "spctl") {
		return []byte("rejected: source=no usable signature"), errors.New("Gatekeeper rejected ad-hoc signature")
	}
	return r.output, r.err
}

func TestVerifierAcceptsLinuxWithoutNativeTool(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner).Verify(context.Background(), "/release/pb", "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("called native verifier for linux: %#v", runner.calls)
	}
}

func TestVerifierAcceptsDarwinCodeSignatureWithoutGatekeeperOverride(t *testing.T) {
	for _, path := range []string{"/release/pb", "/release/renamed.pkg"} {
		t.Run(path, func(t *testing.T) {
			runner := &fakeRunner{rejectGatekeeper: true}
			if err := New(runner).Verify(context.Background(), path, "darwin", "arm64"); err != nil {
				t.Fatal(err)
			}
			if len(runner.calls) != 1 || runner.calls[0].name != "codesign" {
				t.Fatalf("native calls = %#v", runner.calls)
			}
			if got := strings.Join(runner.calls[0].args, " "); got != "--verify --deep --strict --verbose=2 "+path {
				t.Fatalf("codesign args = %q", got)
			}
		})
	}
}

func TestVerifierRejectsCorruptDarwinCodeSignature(t *testing.T) {
	runner := &fakeRunner{output: []byte("code or signature modified"), err: errors.New("invalid signature")}
	if err := New(runner).Verify(context.Background(), "/release/pb", "darwin", "amd64"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "codesign" {
		t.Fatalf("corrupt signature did not fail at codesign: %#v", runner.calls)
	}
}

func TestVerifierAllowsUnsignedWindowsPEWhenTUFAndPEChecksAreUsed(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner).Verify(context.Background(), "C:\\Program Files\\Paperboat\\pb.exe", "windows", "amd64"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unexpected native calls = %#v", runner.calls)
	}
}

func TestVerifierRejectsInvalidAuthenticodeAndUnsupportedPlatforms(t *testing.T) {
	for name, test := range map[string]struct {
		output       []byte
		platform     string
		architecture string
	}{
		"unsupported platform":     {platform: "plan9", architecture: "amd64"},
		"unsupported architecture": {platform: "linux", architecture: "386"},
	} {
		t.Run(name, func(t *testing.T) {
			err := New(&fakeRunner{output: test.output}).Verify(context.Background(), "/release/pb", test.platform, test.architecture)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestUnsignedWindowsReleaseNeverQueriesAuthenticode(t *testing.T) {
	runner := &fakeRunner{err: errors.New("must not run")}
	if err := New(runner).VerifyRelease(context.Background(), `C:\pb.exe`, "windows", "amd64", false); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("an unmarked release was Authenticode-checked: %#v", runner.calls)
	}
}

func TestMarkedWindowsReleaseRequiresAValidSignatureFromTheExpectedPublisher(t *testing.T) {
	publisher := buildinfo.WindowsPublisher
	for name, test := range map[string]struct {
		output string
		err    error
		valid  bool
	}{
		"valid and expected": {output: `{"status":"Valid","publisher":"` + publisher + `"}`, valid: true},
		"unsigned":           {output: `{"status":"NotSigned","publisher":""}`},
		"tampered":           {output: `{"status":"HashMismatch","publisher":"` + publisher + `"}`},
		"untrusted chain":    {output: `{"status":"UnknownError","publisher":"` + publisher + `"}`},
		"other publisher":    {output: `{"status":"Valid","publisher":"Evil ` + publisher + ` Co"}`},
		"unreadable":         {output: `not json`},
		"query failed":       {err: errors.New("powershell failed")},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{output: []byte(test.output), err: test.err}
			err := New(runner).VerifyRelease(context.Background(), `C:\Users\o'brien\pb.exe`, "windows", "arm64", true)
			if test.valid != (err == nil) || (err != nil && !errors.Is(err, ErrInvalid)) {
				t.Fatalf("error = %v", err)
			}
			if len(runner.calls) != 1 || runner.calls[0].name != "powershell.exe" {
				t.Fatalf("calls = %#v", runner.calls)
			}
			// The path is base64 inside the script, so a quote in a file name cannot reach the parser.
			if script := strings.Join(runner.calls[0].args, " "); strings.Contains(script, "brien") {
				t.Fatalf("path leaked into the script text: %s", script)
			}
		})
	}
}

func TestSignedMarkOnlyAffectsWindows(t *testing.T) {
	runner := &fakeRunner{}
	if err := New(runner).VerifyRelease(context.Background(), "/release/pb", "linux", "amd64", true); err != nil || len(runner.calls) != 0 {
		t.Fatalf("linux release: %v %#v", err, runner.calls)
	}
}

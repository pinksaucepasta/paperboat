package nativesignature

import (
	"context"
	"errors"
	"strings"
	"testing"
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
	runner := &fakeRunner{rejectGatekeeper: true}
	if err := New(runner).Verify(context.Background(), "/release/pb", "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "codesign" {
		t.Fatalf("native calls = %#v", runner.calls)
	}
	if got := strings.Join(runner.calls[0].args, " "); got != "--verify --deep --strict --verbose=2 /release/pb" {
		t.Fatalf("codesign args = %q", got)
	}
}

func TestVerifierAcceptsValidDarwinPackageSignatureWithoutNotarization(t *testing.T) {
	runner := &fakeRunner{output: []byte("Status: signed by a certificate trusted by macOS"), rejectGatekeeper: true}
	if err := New(runner).Verify(context.Background(), "/release/pb-darwin-arm64.pkg", "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "/usr/sbin/pkgutil" {
		t.Fatalf("native calls = %#v", runner.calls)
	}
	if got := strings.Join(runner.calls[0].args, " "); got != "--check-signature /release/pb-darwin-arm64.pkg" {
		t.Fatalf("pkgutil args = %q", got)
	}
}

func TestVerifierAcceptsUnsignedDevelopmentDarwinPackage(t *testing.T) {
	runner := &fakeRunner{output: []byte(`Package "pb.pkg": no signature found`), err: errors.New("unsigned")}
	if err := New(runner).Verify(context.Background(), "/release/pb-darwin-arm64.pkg", "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "/usr/sbin/pkgutil" {
		t.Fatalf("native calls = %#v", runner.calls)
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

func TestVerifierRejectsCorruptDarwinPackageSignature(t *testing.T) {
	runner := &fakeRunner{output: []byte("Status: invalid signature"), err: errors.New("package signature verification failed")}
	if err := New(runner).Verify(context.Background(), "/release/pb-darwin-arm64.pkg", "darwin", "arm64"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("corrupt package signature accepted: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "/usr/sbin/pkgutil" {
		t.Fatalf("corrupt package signature did not fail at pkgutil: %#v", runner.calls)
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

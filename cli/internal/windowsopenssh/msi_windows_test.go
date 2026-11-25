//go:build windows

package windowsopenssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// msiRunner records commands; a command whose text contains a key of fail exits with that code.
type msiRunner struct {
	t     *testing.T
	fail  map[string]int
	calls []string
}

func (r *msiRunner) Run(_ context.Context, name string, arguments ...string) ([]byte, error) {
	line := filepath.Base(name) + " " + strings.Join(arguments, " ")
	r.calls = append(r.calls, line)
	for text, code := range r.fail {
		if strings.Contains(line, text) {
			return nil, commandExitFailure(r.t, code)
		}
	}
	return nil, nil
}

func (r *msiRunner) ran(text string) bool {
	return slices.ContainsFunc(r.calls, func(line string) bool { return strings.Contains(line, text) })
}

func TestMSIInstall(t *testing.T) {
	for name, test := range map[string]struct {
		body     string
		fail     map[string]int
		force    bool
		want     error
		message  string
		executed bool // msiexec /i ran
	}{
		"approved":                {body: "approved installer", executed: true},
		"restart pending":         {body: "approved installer", fail: map[string]int{" /i ": 3010}, executed: true},
		"tampered":                {body: "approved installeR", want: ErrUntrustedBinary, message: "not the approved file"},
		"truncated":               {body: "approved", want: ErrUntrustedBinary, message: "not the approved file"},
		"too large":               {body: strings.Repeat("x", maxMSISize+1), want: ErrUntrustedBinary, message: "not the approved file"},
		"not signed by Microsoft": {body: "approved installer", fail: map[string]int{"Get-AuthenticodeSignature": 41}, want: ErrUntrustedBinary, message: "not signed by Microsoft"},
		"install failed":          {body: "approved installer", fail: map[string]int{" /i ": 1603}, executed: true, want: ErrInstallFailed, message: "could not install"},
		"installer busy":          {body: "approved installer", fail: map[string]int{" /i ": 1618}, executed: true, want: ErrInstallFailed, message: "busy"},
		"repair":                  {body: "approved installer", force: true},
		"repair, not installed":   {body: "approved installer", force: true, fail: map[string]int{" /fa ": 1605}, executed: true},
		"repair failed":           {body: "approved installer", force: true, fail: map[string]int{" /fa ": 1603}, want: ErrInstallFailed, message: "could not repair"},
	} {
		t.Run(name, func(t *testing.T) {
			root := serveMSI(t, test.body, nil)
			stale := filepath.Join(root, "openssh-installer", "openssh.msi") // left by a killed install
			if err := errors.Join(os.Mkdir(filepath.Dir(stale), 0o700), os.WriteFile(stale, []byte("stale"), 0o600)); err != nil {
				t.Fatal(err)
			}
			runner := &msiRunner{t: t, fail: test.fail}
			err := installWithMSI(context.Background(), Config{Runner: runner}, test.force)
			if !errors.Is(err, test.want) || (err != nil) != (test.want != nil) || err != nil && !strings.Contains(err.Error(), test.message) {
				t.Fatalf("install = %v, want %v %q", err, test.want, test.message)
			}
			if runner.ran("msiexec.exe /i ") != test.executed || !test.force && runner.ran(" /fa ") || test.want == ErrUntrustedBinary && runner.ran("msiexec") {
				t.Fatalf("commands = %q", runner.calls)
			}
			if entries, readErr := os.ReadDir(root); readErr != nil || len(entries) != 0 {
				t.Fatalf("installer left behind: %v %v", entries, readErr)
			}
		})
	}
}

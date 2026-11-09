package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMacOSInstallerRunsOnlyVerifiedDirectProduct(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets POSIX hosts (Windows installs with install.ps1); under Git Bash its fixtures see MSYS paths")
	}
	for _, scenario := range []string{"valid", "no-join", "wrong-hash", "wrong-length", "wrong-url", "install-failure", "reset-failure"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			product := filepath.Join(root, "product")
			calls, canonical := filepath.Join(root, "calls"), filepath.Join(root, "installed")
			fixture := `#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$CALLS"
case "$1" in
reset)
  printf '{"ok":false,"error":{"message":"private fixture detail"}}\n'
  exit 19 ;;
install)
  if [ "$SCENARIO" = install-failure ]; then
    printf '{"ok":false,"error":{"message":"private fixture detail"}}\n'
    exit 19
  fi
  cp "$0" "$CANONICAL"
  chmod 700 "$CANONICAL"
  printf '{"ok":true,"data":{"executable":"%s"}}\n' "$CANONICAL" ;;
login) ;;
--version) printf 'fixture version\n' ;;
*) exit 19 ;;
esac
`
			writeInstallerFixture(t, product, fixture)
			digest := sha256.Sum256([]byte(fixture))
			values := map[string]string{
				"PAPERBOAT_PRODUCT_DARWIN_ARM64_VERSION": "2026.10.15.1",
				"PAPERBOAT_PRODUCT_DARWIN_ARM64_URL":     "https://github.com/example/paperboat/releases/download/2026.10.15.1/pb-darwin-arm64",
				"PAPERBOAT_PRODUCT_DARWIN_ARM64_SHA256":  hex.EncodeToString(digest[:]),
				"PAPERBOAT_PRODUCT_DARWIN_ARM64_LENGTH":  fmt.Sprint(len(fixture)),
				"PAPERBOAT_PRODUCT_REPOSITORY":           "example/paperboat",
			}
			switch scenario {
			case "wrong-hash":
				values["PAPERBOAT_PRODUCT_DARWIN_ARM64_SHA256"] = strings.Repeat("f", 64)
			case "wrong-length":
				values["PAPERBOAT_PRODUCT_DARWIN_ARM64_LENGTH"] = fmt.Sprint(len(fixture) + 1)
			case "wrong-url":
				values["PAPERBOAT_PRODUCT_DARWIN_ARM64_URL"] = "https://example.test/untrusted"
			}
			body, err := os.ReadFile("install.sh")
			if err != nil {
				t.Fatal(err)
			}
			script := string(body)
			for key, value := range values {
				script = strings.ReplaceAll(script, "@"+key+"@", value)
			}
			installer := filepath.Join(root, "install.sh")
			writeInstallerFixture(t, installer, script)
			writeInstallerFixture(t, filepath.Join(root, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo Darwin ;; -m) echo arm64 ;; *) exit 1 ;; esac\n")
			writeInstallerFixture(t, filepath.Join(root, "curl"), "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then cp \"$PRODUCT\" \"$2\"; exit; fi; shift; done\nexit 1\n")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			args := []string{installer}
			if scenario == "reset-failure" {
				args = append(args, "--enrollment-token", "fixture-token")
			}
			if scenario == "no-join" {
				args = append(args, "--no-setup")
			}
			command := exec.CommandContext(ctx, "sh", args...)
			command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"),
				"TMPDIR="+root, "SCENARIO="+scenario, "PRODUCT="+product, "CALLS="+calls, "CANONICAL="+canonical,
				"PAPERBOAT_INSTALL_DIR=", "PAPERBOAT_ENROLLMENT_TOKEN=")
			output, err := command.CombinedOutput()
			if scenario == "valid" || scenario == "no-join" {
				if err != nil {
					t.Fatalf("verified direct product failed: %v: %s", err, output)
				}
				got, err := os.ReadFile(calls)
				// Without a token the installer goes straight on to the browser approval that adds
				// the computer (pb login); --no-setup only installs.
				want := map[string]string{"valid": "install\nlogin\n", "no-join": "install\n--version\n"}[scenario]
				if err != nil || string(got) != want {
					t.Fatalf("supplied install then canonical executable: %q, want %q, %v", got, want, err)
				}
			} else if scenario == "install-failure" || scenario == "reset-failure" {
				step := strings.TrimSuffix(scenario, "-failure")
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 19 {
					t.Fatalf("%s failure status = %v, output=%s", step, err, output)
				}
				if !strings.Contains(string(output), "pb "+step+" failed") || !strings.Contains(string(output), "rerun this installer") {
					t.Fatalf("%s failed without step and recovery: %q", step, output)
				}
				if strings.Contains(string(output), "fixture-token") {
					t.Fatal("installer exposed the enrollment token")
				}
				// A failed reset names pb's own reason (pbmac F1-01 printed only "pb reset failed").
				if scenario == "reset-failure" && !strings.Contains(string(output), "pb reset failed: private fixture detail;") {
					t.Fatalf("reset failure did not say why: %q", output)
				}
				if scenario == "install-failure" && strings.Contains(string(output), "private fixture detail") {
					t.Fatal("installer exposed the install step's captured details")
				}
				got, readErr := os.ReadFile(calls)
				if readErr != nil || string(got) != step+"\n" {
					t.Fatalf("installer continued after %s failure: %q, %v", step, got, readErr)
				}
			} else if err == nil {
				t.Fatal("unverified product was accepted")
			} else if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatal("unverified product was executed")
			}
			staging, _ := filepath.Glob(filepath.Join(root, "paperboat-install.*"))
			if len(staging) != 0 {
				t.Fatal("installer retained downloaded staging")
			}
		})
	}
}

// F1-01 (pbmac, 2026-11-06): macOS TMPDIR ends with "/", the installer staged its token file at ".../T//paperboat-install.X/enrollment-token"
// and pb reset rejected the unclean path with a bare "could not finish the command".
func TestInstallerStagesACleanTokenFilePathWhenTMPDIRHasATrailingSlash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets POSIX hosts (Windows installs with install.ps1); under Git Bash its fixtures see MSYS paths")
	}
	root := t.TempDir()
	spaced := filepath.Join(root, "a b")
	if err := os.Mkdir(spaced, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, tmpdir := range map[string]string{
		"unset":             "",
		"trailing slash":    root + "/",
		"repeated trailing": root + "//",
		"leading and tail":  "/" + root + "//",
		"spaces":            spaced + "//",
	} {
		t.Run(name, func(t *testing.T) { checkInstallerStagesCleanTokenPath(t, tmpdir) })
	}
}

func checkInstallerStagesCleanTokenPath(t *testing.T, tmpdir string) {
	root := t.TempDir()
	product, seen := filepath.Join(root, "product"), filepath.Join(root, "token-path")
	fixture := `#!/bin/sh
set -eu
case "$1" in
reset) shift; while [ "$#" -gt 0 ]; do [ "$1" = --enrollment-token-file ] && printf '%s' "$2" > "$SEEN"; shift; done; exit 19 ;;
*) exit 19 ;;
esac
`
	writeInstallerFixture(t, product, fixture)
	digest := sha256.Sum256([]byte(fixture))
	body, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for key, value := range map[string]string{
		"PAPERBOAT_PRODUCT_DARWIN_ARM64_VERSION": "2026.10.15.1",
		"PAPERBOAT_PRODUCT_DARWIN_ARM64_URL":     "https://github.com/example/paperboat/releases/download/2026.10.15.1/pb-darwin-arm64",
		"PAPERBOAT_PRODUCT_DARWIN_ARM64_SHA256":  hex.EncodeToString(digest[:]),
		"PAPERBOAT_PRODUCT_DARWIN_ARM64_LENGTH":  fmt.Sprint(len(fixture)),
		"PAPERBOAT_PRODUCT_REPOSITORY":           "example/paperboat",
	} {
		script = strings.ReplaceAll(script, "@"+key+"@", value)
	}
	installer := filepath.Join(root, "install.sh")
	writeInstallerFixture(t, installer, script)
	writeInstallerFixture(t, filepath.Join(root, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo Darwin ;; -m) echo arm64 ;; *) exit 1 ;; esac\n")
	writeInstallerFixture(t, filepath.Join(root, "curl"), "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then cp \"$PRODUCT\" \"$2\"; exit; fi; shift; done\nexit 1\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sh", installer, "--enrollment-token", "fixture-token")
	command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "TMPDIR="+tmpdir, "PRODUCT="+product, "SEEN="+seen,
		"PAPERBOAT_INSTALL_DIR=", "PAPERBOAT_ENROLLMENT_TOKEN=")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("fixture reset exits 19, installer succeeded: %s", output)
	}
	path, err := os.ReadFile(seen)
	if err != nil || len(path) == 0 {
		t.Fatalf("reset was not given a token file: %v", err)
	}
	if filepath.Clean(string(path)) != string(path) {
		t.Fatalf("token file path is not clean: %q", path)
	}
}

func writeInstallerFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

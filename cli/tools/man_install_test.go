package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLinuxInstallerRunsVerifiedPublicInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer")
	}
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "tools")
			prefix := filepath.Join(dir, "prefix")
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			payload := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PB_TEST_LOG\"\nif [ \"$1\" = install ] && [ \"$2\" = --install-dir ] && [ \"$4\" = --json ]; then mkdir -p \"$3\"; cp \"$0\" \"$3/pb\"; chmod 0755 \"$3/pb\"; printf '{\"ok\":true,\"data\":{\"executable\":\"%s\"}}\\n' \"$3/pb\"; fi\n"
			write(filepath.Join(dir, "binary"), payload)
			verifier := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do case \"$1\" in --state-dir) state=$2; shift 2;; *) shift;; esac; done\nmkdir -p \"$state/product\"\ncp \"$PB_TEST_ROOT/binary\" \"$state/product/pb-linux-amd64\"\nprintf '{\"path\":\"%s\",\"version\":\"2026.09.19.1\"}\\n' \"$state/product/pb-linux-amd64\"\n"
			write(filepath.Join(dir, "verifier"), verifier)
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(verifier)))
			if !valid {
				digest = strings.Repeat("0", 64)
			}
			body, err := os.ReadFile("install.sh")
			if err != nil {
				t.Fatal(err)
			}
			rendered := strings.ReplaceAll(string(body), "@PAPERBOAT_BOOTSTRAP_VERSION@", "2026.09.19.1")
			rendered = strings.ReplaceAll(rendered, "@PAPERBOAT_BOOTSTRAP_REPOSITORY@", "pinksaucepasta/paperboat-cli")
			rendered = strings.ReplaceAll(rendered, "@PAPERBOAT_BOOTSTRAP_LINUX_AMD64_SHA256@", digest)
			rendered = strings.ReplaceAll(rendered, "@PAPERBOAT_BOOTSTRAP_LINUX_AMD64_LENGTH@", fmt.Sprint(len(verifier)))
			installer := filepath.Join(dir, "install")
			write(installer, rendered)
			write(filepath.Join(bin, "curl"), "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then cp \"$PB_TEST_ROOT/verifier\" \"$2\"; exit; fi; shift; done\nexit 1\n")
			write(filepath.Join(bin, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac\n")
			write(filepath.Join(bin, "id"), "#!/bin/sh\necho 1000\n")
			write(filepath.Join(bin, "sudo"), "#!/bin/sh\n[ \"$*\" = '-n true' ]\n")
			command := exec.Command("sh", installer, "--no-setup", "--install-dir", filepath.Join(prefix, "bin"))
			command.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "PB_TEST_ROOT="+dir, "PB_TEST_LOG="+filepath.Join(dir, "calls"), "PAPERBOAT_ENROLLMENT_TOKEN=", "PAPERBOAT_MACHINE_NAME=", "PAPERBOAT_VERSION=latest", "PAPERBOAT_GITHUB_REPOSITORY=pinksaucepasta/paperboat-cli")
			output, err := command.CombinedOutput()
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if !valid {
				if err == nil || len(calls) != 0 {
					t.Fatalf("unverified payload executed: %v %s %s", err, calls, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("installer: %v %s", err, output)
			}
			want := "install --install-dir " + filepath.Join(prefix, "bin") + " --json\n--version\n"
			if string(calls) != want {
				t.Fatalf("calls %q; want %q", calls, want)
			}
		})
	}
}

func TestPairWithoutTokenFailsBeforeDownloadOrReset(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "curl-called")
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\ntouch \"$PB_TEST_LOG\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "install.sh", "--pair")
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "PB_TEST_LOG="+log, "PAPERBOAT_ENROLLMENT_TOKEN=", "PAPERBOAT_ENROLLMENT_TOKEN_FILE=")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--pair requires") {
		t.Fatalf("error = %v, output = %s", err, output)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("download ran before missing-token rejection: %v", err)
	}
}

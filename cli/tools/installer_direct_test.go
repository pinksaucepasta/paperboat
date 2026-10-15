package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMacOSInstallerRunsOnlyVerifiedDirectProduct(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-hash", "wrong-length", "wrong-url"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			product := filepath.Join(root, "product")
			calls, canonical := filepath.Join(root, "calls"), filepath.Join(root, "installed")
			fixture := `#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$CALLS"
case "$1" in
install)
  cp "$0" "$CANONICAL"
  chmod 700 "$CANONICAL"
  printf '{"ok":true,"data":{"executable":"%s"}}\n' "$CANONICAL" ;;
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
			command := exec.CommandContext(ctx, "sh", installer)
			command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"),
				"TMPDIR="+root, "PRODUCT="+product, "CALLS="+calls, "CANONICAL="+canonical,
				"PAPERBOAT_INSTALL_DIR=", "PAPERBOAT_ENROLLMENT_TOKEN=")
			output, err := command.CombinedOutput()
			if scenario == "valid" {
				if err != nil {
					t.Fatalf("verified direct product failed: %v: %s", err, output)
				}
				got, err := os.ReadFile(calls)
				if err != nil || string(got) != "install\n--version\n" {
					t.Fatalf("supplied install then canonical executable: %q, %v", got, err)
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

func writeInstallerFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixturePB is the "pb" the installer downloads: it records its calls and answers install like the real one.
const fixturePB = `#!/bin/sh
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
pair) printf '%s\n' "$PB_RECIPE" >> "$CALLS.recipe" ;;
--version) printf 'fixture version\n' ;;
*) exit 19 ;;
esac
`

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets POSIX hosts (Windows installs with install.ps1); under Git Bash its fixtures see MSYS paths")
	}
}

func macRelease(t *testing.T) *fakeRelease {
	return newFakeRelease(t, "Darwin", "arm64", "pb-darwin-arm64", fixturePB)
}

func TestInstallerRunsOnlyTheVerifiedProduct(t *testing.T) {
	skipOnWindows(t)
	for _, scenario := range []string{"valid", "no-join", "wrong-hash", "install-failure", "reset-failure"} {
		t.Run(scenario, func(t *testing.T) {
			r := macRelease(t)
			calls := filepath.Join(r.root, "calls")
			if scenario == "wrong-hash" {
				write(t, r.file("pb-darwin-arm64"), strings.Replace(fixturePB, "fixture version", "tampered version", 1), 0o700)
			}
			args := []string{}
			switch scenario {
			case "no-join":
				args = append(args, "--no-setup")
			case "reset-failure":
				args = append(args, "--enrollment-token", "fixture-token")
			}
			output, err := r.run(t, args, "SCENARIO="+scenario, "CALLS="+calls, "CANONICAL="+filepath.Join(r.root, "installed"))
			switch scenario {
			case "valid", "no-join":
				if err != nil {
					t.Fatalf("verified product failed: %v: %s", err, output)
				}
				// Without a token the installer goes straight on to the browser approval that adds the computer (pb login).
				want := map[string]string{"valid": "install\nlogin\n", "no-join": "install\n--version\n"}[scenario]
				if got, readErr := os.ReadFile(calls); readErr != nil || string(got) != want {
					t.Fatalf("calls = %q, want %q (%v)", got, want, readErr)
				}
			case "install-failure", "reset-failure":
				step := strings.TrimSuffix(scenario, "-failure")
				if err == nil || !strings.Contains(output, "pb "+step+" failed") || !strings.Contains(output, "rerun this installer") {
					t.Fatalf("%s failed without step and recovery: %v %q", step, err, output)
				}
				if strings.Contains(output, "fixture-token") {
					t.Fatal("installer exposed the enrollment token")
				}
				// A failed reset names pb's own reason (a test Mac F1-01 printed only "pb reset failed").
				if scenario == "reset-failure" && !strings.Contains(output, "pb reset failed: private fixture detail;") {
					t.Fatalf("reset failure did not say why: %q", output)
				}
				if scenario == "install-failure" && strings.Contains(output, "private fixture detail") {
					t.Fatal("installer exposed the install step's captured details")
				}
				if got, _ := os.ReadFile(calls); string(got) != step+"\n" {
					t.Fatalf("installer continued after %s failure: %q", step, got)
				}
			case "wrong-hash":
				if err == nil || !strings.Contains(output, "product digest mismatch") {
					t.Fatalf("tampered product accepted: %v %s", err, output)
				}
				if _, statErr := os.Stat(calls); !os.IsNotExist(statErr) {
					t.Fatal("tampered product was executed")
				}
			}
			if r.stagingLeft(t) {
				t.Fatal("installer retained downloaded staging")
			}
		})
	}
}

// The latest release is whatever GitHub redirects /releases/latest to; PB_VERSION and --version skip that lookup.
func TestInstallerResolvesLatestOrTheRequestedVersion(t *testing.T) {
	skipOnWindows(t)
	r := macRelease(t)
	env := []string{"SCENARIO=ok", "CALLS=" + filepath.Join(r.root, "calls"), "CANONICAL=" + filepath.Join(r.root, "installed")}
	if out, err := r.run(t, []string{"--no-setup"}, env...); err != nil {
		t.Fatalf("latest: %v %s", err, out)
	}
	if got := r.downloads(t); got[0] != "https://github.com/"+fakeRepo+"/releases/latest" || got[1] != "https://github.com/"+fakeRepo+"/releases/download/"+fakeVersion+"/SHA256SUMS" {
		t.Fatalf("latest resolution downloads = %q", got)
	}
	_ = os.Remove(filepath.Join(r.root, "curl-log"))
	if out, err := r.run(t, []string{"--no-setup"}, append(env, "PB_VERSION="+fakeVersion, "FAKE_LATEST=2030.01.01.1")...); err != nil {
		t.Fatalf("PB_VERSION: %v %s", err, out)
	}
	for _, url := range r.downloads(t) {
		if strings.HasSuffix(url, "/releases/latest") {
			t.Fatalf("a pinned version still looked up the latest release: %q", r.downloads(t))
		}
	}
	if out, err := r.run(t, []string{"--no-setup", "--version", "2026.11.18.9"}, env...); err == nil || !strings.Contains(out, "could not download SHA256SUMS for 2026.11.18.9") {
		t.Fatalf("an unpublished version was not refused: %v %s", err, out)
	}
	for _, bad := range []string{"latest-ish", "2026.11.18", "2026.11.18.01", "../../x"} {
		if out, err := r.run(t, []string{"--no-setup", "--version", bad}, env...); err == nil || !strings.Contains(out, "is not a release version") {
			t.Fatalf("version %q was not refused: %v %s", bad, err, out)
		}
	}
	if out, err := r.run(t, []string{"--no-setup"}, append(env, "PB_REPO=not a repo")...); err == nil || !strings.Contains(out, "PB_REPO must look like owner/repo") {
		t.Fatalf("a bad PB_REPO was not refused: %v %s", err, out)
	}
}

// Nothing is trusted before the signature checks out; each failure stops before pb runs.
func TestInstallerRefusesAReleaseWhoseSignatureOrIdentityIsWrong(t *testing.T) {
	skipOnWindows(t)
	cases := map[string]struct {
		damage func(t *testing.T, r *fakeRelease)
		want   string
	}{
		"sums edited after signing": {func(t *testing.T, r *fakeRelease) {
			body, _ := os.ReadFile(r.file("SHA256SUMS"))
			write(t, r.file("SHA256SUMS"), strings.Replace(string(body), "0", "1", 1), 0o644)
		}, "release signature on SHA256SUMS is not valid"},
		"signature missing": {func(t *testing.T, r *fakeRelease) { _ = os.Remove(r.file("SHA256SUMS.sig")) }, "could not download SHA256SUMS.sig"},
		"signed by another key": {func(t *testing.T, r *fakeRelease) {
			other := filepath.Join(r.root, "other-key")
			if out, err := execCommand("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", other); err != nil {
				t.Fatalf("ssh-keygen: %v %s", err, out)
			}
			_ = os.Remove(r.file("SHA256SUMS.sig")) // ssh-keygen -Y sign silently keeps an existing .sig
			if out, err := execCommand("ssh-keygen", "-q", "-Y", "sign", "-f", other, "-n", "paperboat-release", r.file("SHA256SUMS")); err != nil {
				t.Fatalf("sign: %v %s", err, out)
			}
		}, "release signature on SHA256SUMS is not valid"},
		"signed for another release": {func(t *testing.T, r *fakeRelease) {
			// A genuine signed SHA256SUMS of an older release, replayed under the new tag.
			older := filepath.Join(r.releases, "2026.11.01.0")
			if err := os.MkdirAll(older, 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(older, "pb-darwin-arm64"), fixturePB, 0o700)
			r.sign(t, "2026.11.01.0", "")
			for _, name := range []string{"SHA256SUMS", "SHA256SUMS.sig"} {
				body, _ := os.ReadFile(filepath.Join(older, name))
				write(t, r.file(name), string(body), 0o644)
			}
		}, "signed for a different release"},
		"asset not listed": {func(t *testing.T, r *fakeRelease) {
			write(t, filepath.Join(r.releases, r.version, "pb-darwin-arm64"), fixturePB, 0o700)
			if err := os.Rename(r.file("pb-darwin-arm64"), r.file("pb-other")); err != nil {
				t.Fatal(err)
			}
			r.sign(t, r.version, "")
			write(t, r.file("pb-darwin-arm64"), fixturePB, 0o700)
		}, "does not list pb-darwin-arm64 exactly once"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := macRelease(t)
			c.damage(t, r)
			calls := filepath.Join(r.root, "calls")
			output, err := r.run(t, nil, "SCENARIO=ok", "CALLS="+calls, "CANONICAL="+filepath.Join(r.root, "installed"))
			if err == nil || !strings.Contains(output, c.want) {
				t.Fatalf("want refusal %q: %v %s", c.want, err, output)
			}
			if _, statErr := os.Stat(calls); !os.IsNotExist(statErr) {
				t.Fatal("pb ran although the release was refused")
			}
			for _, url := range r.downloads(t) {
				if strings.HasSuffix(url, "/pb-darwin-arm64") {
					t.Fatalf("the product was downloaded before the signature was trusted: %q", r.downloads(t))
				}
			}
		})
	}
}

// F1-01 (a test Mac, 2026-11-06): macOS TMPDIR ends with "/", the installer staged its token file at ".../T//paperboat-install.X/enrollment-token"
// and pb reset rejected the unclean path with a bare "could not finish the command".
func TestInstallerStagesACleanTokenFilePathWhenTMPDIRHasATrailingSlash(t *testing.T) {
	skipOnWindows(t)
	for name, tmpdir := range map[string]func(root string) string{
		"unset":             func(string) string { return "" },
		"trailing slash":    func(root string) string { return root + "/" },
		"repeated trailing": func(root string) string { return root + "//" },
		"leading and tail":  func(root string) string { return "/" + root + "//" },
		"spaces": func(root string) string {
			spaced := filepath.Join(root, "a b")
			if err := os.Mkdir(spaced, 0o700); err != nil {
				t.Fatal(err)
			}
			return spaced + "//"
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := "#!/bin/sh\nset -eu\ncase \"$1\" in\nreset) shift; while [ \"$#\" -gt 0 ]; do [ \"$1\" = --enrollment-token-file ] && printf '%s' \"$2\" > \"$SEEN\"; shift; done; exit 19 ;;\n*) exit 19 ;;\nesac\n"
			r := newFakeRelease(t, "Darwin", "arm64", "pb-darwin-arm64", fixture)
			seen := filepath.Join(r.root, "token-path")
			output, err := r.run(t, []string{"--enrollment-token", "fixture-token"}, "TMPDIR="+tmpdir(r.root), "SEEN="+seen)
			if err == nil {
				t.Fatalf("fixture reset exits 19, installer succeeded: %s", output)
			}
			path, readErr := os.ReadFile(seen)
			if readErr != nil || len(path) == 0 {
				t.Fatalf("reset was not given a token file: %v %s", readErr, output)
			}
			if filepath.Clean(string(path)) != string(path) {
				t.Fatalf("token file path is not clean: %q", path)
			}
		})
	}
}

// PB_RECIPE pairs from a recipe token; a malformed one is refused before anything is downloaded.
func TestInstallerPairsFromARecipeAndRefusesAMalformedToken(t *testing.T) {
	skipOnWindows(t)
	r := macRelease(t)
	for value, want := range map[string]string{
		"pbr_short":              "this value is 9",
		"not-a-token-at-all-xyz": "PB_RECIPE is not a recipe token",
	} {
		output, err := r.run(t, nil, "PB_RECIPE="+value)
		if err == nil || !strings.Contains(output, want) || len(r.downloads(t)) != 0 {
			t.Fatalf("PB_RECIPE=%q: err=%v downloads=%q output=%s", value, err, r.downloads(t), output)
		}
	}
	token := "pbr_" + strings.Repeat("a", 63)
	calls := filepath.Join(r.root, "calls")
	if output, err := r.run(t, nil, "PB_RECIPE="+token, "SCENARIO=ok", "CALLS="+calls, "CANONICAL="+filepath.Join(r.root, "installed")); err != nil {
		t.Fatalf("recipe install: %v %s", err, output)
	}
	if got, _ := os.ReadFile(calls); string(got) != "install\npair\n" {
		t.Fatalf("recipe install calls = %q, want install then pair (never login)", got)
	}
	if got, _ := os.ReadFile(calls + ".recipe"); strings.TrimSpace(string(got)) != token {
		t.Fatal("pair did not receive PB_RECIPE")
	}
}

func TestInstallerTakesTheEnrollmentTokenFromPBTokenNeverFromArguments(t *testing.T) {
	skipOnWindows(t)
	fixture := "#!/bin/sh\nset -eu\ncase \"$1\" in\nreset) shift; while [ \"$#\" -gt 0 ]; do case \"$1\" in --enrollment-token-file) cp \"$2\" \"$SEEN\" ;; esac; shift; done; exit 19 ;;\n*) exit 19 ;;\nesac\n"
	r := newFakeRelease(t, "Darwin", "arm64", "pb-darwin-arm64", fixture)
	seen := filepath.Join(r.root, "token")
	if _, err := r.run(t, nil, "PB_TOKEN=ABCDEFGHIJKLMNOPQRSTUVWXYZ", "PB_NAME=laptop", "SEEN="+seen); err == nil {
		t.Fatal("fixture reset exits 19")
	}
	if got, _ := os.ReadFile(seen); string(got) != "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Fatalf("pb reset was not given the PB_TOKEN token: %q", got)
	}
}

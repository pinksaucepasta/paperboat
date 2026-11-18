package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeRelease is a signed release on disk plus a curl that serves it as GitHub does: the installer is the repository's
// install.sh with a throwaway signing key swapped in for the embedded one, so every test checks the real script.
type fakeRelease struct {
	root, bin, installer, powershell, releases string
	version                                    string
	path                                       string // when set, the whole PATH the installer sees (to leave tools out)
}

const (
	fakeRepo    = "example/paperboat"
	fakeVersion = "2026.11.18.1"
)

// The shim answers the two GitHub URLs the installer uses: /releases/latest (a redirect to the newest tag) and
// /releases/download/<version>/<file>. Anything else is a 404 (curl exit 22).
const curlShim = `#!/bin/sh
out= url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -w|--connect-timeout|--max-time|--max-filesize|--proto|--proto-redir) shift 2 ;;
    --*) shift ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$FAKE_CURL_LOG"
case "$url" in
  https://github.com/"$FAKE_REPO"/releases/latest) printf '%s' "https://github.com/$FAKE_REPO/releases/tag/$FAKE_LATEST" ;;
  https://github.com/"$FAKE_REPO"/releases/download/*/*)
    file=${url##*/}; version=${url%/*}; version=${version##*/}
    [ -f "$FAKE_RELEASES/$version/$file" ] || exit 22
    cp "$FAKE_RELEASES/$version/$file" "$out" ;;
  *) exit 22 ;;
esac
`

func newFakeRelease(t *testing.T, osName, arch, asset, payload string) *fakeRelease {
	t.Helper()
	for _, tool := range []string{"ssh-keygen", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	root := t.TempDir()
	r := &fakeRelease{root: root, bin: filepath.Join(root, "bin"), releases: filepath.Join(root, "releases"), version: fakeVersion}
	for _, dir := range []string{r.bin, filepath.Join(r.releases, fakeVersion)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(root, "signing-key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	public, _ := os.ReadFile(key + ".pub")
	fields := strings.Fields(string(public))
	body, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	embedded := regexp.MustCompile(`allowed_signers='[^']*'`)
	if !embedded.Match(body) {
		t.Fatal("install.sh embeds no allowed_signers key")
	}
	patched := embedded.ReplaceAllLiteral(body, []byte(`allowed_signers='paperboat-release namespaces="paperboat-release" `+fields[0]+" "+fields[1]+`'`))
	r.installer = filepath.Join(root, "install.sh")
	write(t, r.installer, string(patched), 0o700)
	// The PowerShell installer embeds the same key; patch its copy the same way.
	ps, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	psKey := regexp.MustCompile(`\$allowedSigners = '[^']*'`)
	if !psKey.Match(ps) {
		t.Fatal("install.ps1 embeds no allowedSigners key")
	}
	r.powershell = filepath.Join(root, "install.ps1")
	write(t, r.powershell, string(psKey.ReplaceAllLiteral(ps, []byte(`$allowedSigners = 'paperboat-release namespaces="paperboat-release" `+fields[0]+" "+fields[1]+`'`))), 0o600)
	write(t, filepath.Join(r.releases, fakeVersion, asset), payload, 0o700)
	r.sign(t, fakeVersion, key)
	write(t, filepath.Join(r.bin, "curl"), curlShim, 0o700)
	write(t, filepath.Join(r.bin, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo "+osName+" ;; -m) echo "+arch+" ;; *) exit 1 ;; esac\n", 0o700)
	return r
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// sign writes SHA256SUMS and SHA256SUMS.sig for a release directory the way CI does (the same script).
func (r *fakeRelease) sign(t *testing.T, version, key string) {
	t.Helper()
	if key == "" {
		key = filepath.Join(r.root, "signing-key")
	}
	if out, err := exec.Command("sh", "sign-release-assets.sh", filepath.Join(r.releases, version), version, key, r.installer, r.powershell).CombinedOutput(); err != nil {
		t.Fatalf("sign-release-assets: %v %s", err, out)
	}
}

// addAsset publishes one more file in the release and signs again.
func (r *fakeRelease) addAsset(t *testing.T, name, body string) {
	t.Helper()
	write(t, r.file(name), body, 0o700)
	r.sign(t, r.version, "")
}

func (r *fakeRelease) file(name string) string { return filepath.Join(r.releases, r.version, name) }

// run executes the installer with the fake PATH. extra is KEY=value pairs; HOME and TMPDIR stay inside the test directory.
func (r *fakeRelease) run(t *testing.T, args []string, extra ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sh", append([]string{r.installer}, args...)...)
	path := r.bin + ":" + os.Getenv("PATH")
	if r.path != "" {
		path = r.path
	}
	command.Env = append(os.Environ(), "PATH="+path, "HOME="+r.root, "TMPDIR="+r.root,
		"FAKE_RELEASES="+r.releases, "FAKE_REPO="+fakeRepo, "FAKE_LATEST="+r.version, "FAKE_CURL_LOG="+filepath.Join(r.root, "curl-log"),
		"PB_REPO="+fakeRepo, "PB_TOKEN=", "PB_RECIPE=", "PB_VERSION=", "PB_NAME=", "PAPERBOAT_INSTALL_DIR=")
	command.Env = append(command.Env, extra...)
	out, err := command.CombinedOutput()
	return string(out), err
}

func (r *fakeRelease) downloads(t *testing.T) []string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(r.root, "curl-log"))
	return strings.Fields(string(data))
}

func (r *fakeRelease) stagingLeft(t *testing.T) bool {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(r.root, "paperboat-install.*"))
	return len(left) != 0
}

func execCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

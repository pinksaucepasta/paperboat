//go:build darwin || linux

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

const tailChildRoot = "PB_TEST_ACTIVATION_TAIL_ROOT"

// The helper refuses to run unless it is the persisted copy at activation/pb, so the tail after the activation
// (diagnostic, retirement) is driven by re-executing this test binary from there.
func TestActivationHelperHoldsTheLockThroughTheActivationTail(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires protected activation custody")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secureChild(root, "activation"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "pb"), filepath.Join(root, "activation", "pb")} {
		if err := os.WriteFile(path, image, 0755); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(image)
	previous := workerupdate.Release{Version: "2026.10.13.1", Platform: runtime.GOOS, Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Length: int64(len(image)), HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1}
	handoff := &unixActivationHandoff{Schema: unixHandoffSchema, Started: true, Candidate: "2026.10.13.2", ApprovalID: strings.Repeat("a", 64), Previous: previous}
	if err := writeUnixHandoff(root, handoff); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(root, "activation", "pb"), "-test.run=^TestActivationHelperTailChild$", "-test.v")
	command.Env = append(os.Environ(), tailChildRoot+"="+root)
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "--- PASS: TestActivationHelperTailChild") {
		t.Fatalf("helper process: %v\n%s", err, output)
	}
	probe, err := os.ReadFile(filepath.Join(root, "probe"))
	if err != nil || string(probe) != ErrActivationPending.Error() {
		t.Fatalf("a queue attempt during the tail's retirement got %q (%v), want %q", probe, err, ErrActivationPending)
	}
	if _, err := os.Stat(unixHandoffPath(root)); !os.IsNotExist(err) {
		t.Fatalf("the handoff was not retired: %v", err)
	}
}

// TestActivationHelperTailChild is the helper process of the test above; it does nothing on its own.
func TestActivationHelperTailChild(t *testing.T) {
	root := os.Getenv(tailChildRoot)
	if root == "" {
		t.Skip("helper process only")
	}
	handoff, err := readUnixHandoff(root)
	if err != nil || handoff == nil {
		t.Fatalf("handoff: %v %v", handoff, err)
	}
	buildinfo.Version = handoff.Previous.Version
	controller := &lockProbingController{root: root}
	service := &Service{config: Config{StateRoot: root, ActivationController: controller, ActivationGate: statusGate{}, Binary: filepath.Join(root, "pb")},
		managerConfig: workerupdate.Config{StatePath: filepath.Join(root, "transaction.json"), Binary: filepath.Join(root, "pb"), BinaryStaged: filepath.Join(root, "staged"),
			OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Fetcher: statusFetcher{}, Hostd: statusHostd{}, Health: statusHealth{},
			ApplyPrepared: func(context.Context, workerupdate.PreparedActivation) (installsource.Source, error) {
				return installsource.Source{}, nil
			}}}
	if err := service.RunActivationHelper(context.Background()); err != nil {
		t.Fatal(err)
	}
	probe := ""
	if controller.probe != nil {
		probe = controller.probe.Error()
	}
	if err := os.WriteFile(filepath.Join(root, "probe"), []byte(probe), 0600); err != nil {
		t.Fatal(err)
	}
}

//go:build darwin || linux

package localdaemon

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type fixedSnapshot localapi.Snapshot

func (s fixedSnapshot) Snapshot(context.Context) (localapi.Snapshot, error) {
	return localapi.Snapshot(s), nil
}

// X-42 on the fleet: the bundle pb bugreport writes still named every machine, because the daemon marshalled its
// snapshot without bugreportStatus. This reads the status.json of a real bundle.
func TestBugreportBundleStatusNamesNoOtherMachine(t *testing.T) {
	service := diagnosticService{recorder: diagnostics.NewMemoryRecorder(), stateRoot: t.TempDir(), ownerUID: os.Getuid(), clock: time.Now,
		snapshots: fixedSnapshot{DaemonState: "ready", Machines: []localapi.MachineStatus{{ID: "machine_other", Alias: "studio-other", OwnerAccount: "acct_other", WorkspaceRoot: "/home/alice", SelectedPath: "direct"}}}}
	bundle, err := service.CreateBugreport(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(bundle.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	file, err := archive.Open("status.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	status := string(raw)
	for _, leaked := range []string{"machine_other", "studio-other", "acct_other", "/home/alice"} {
		if strings.Contains(status, leaked) {
			t.Errorf("the bundle's status.json holds %q: %s", leaked, status)
		}
	}
	if !strings.Contains(status, `"selected_path":"direct"`) {
		t.Errorf("the bundle lost the selected path (F21-06): %s", status)
	}
}

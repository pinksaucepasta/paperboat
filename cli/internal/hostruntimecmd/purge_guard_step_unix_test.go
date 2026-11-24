//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
)

type guardStepFixture struct{ step string }

func (e guardStepFixture) Error() string     { return "PRIVATE " + e.step }
func (e guardStepFixture) PurgeStep() string { return e.step }

// A failed machine guard purge said only "machine_guard": the report names its step, and carries no error text.
func TestPurgeResultNamesTheFailingMachineGuardStep(t *testing.T) {
	for _, step := range []string{"stop", "files", "addresses", "proxy", "trust", "firewall", "account"} {
		root := t.TempDir()
		plantLinuxAccount(t, root)
		plan, err := newUnixPurgePlan("linux", 1001)
		if err != nil {
			t.Fatal(err)
		}
		sweep := testSweep(root, plan, newPurgeFake())
		sweep.guard = func(context.Context) error { return errors.Join(guardStepFixture{step}) }
		purgeErr := sweep.purgeAccount(t.Context())
		var output bytes.Buffer
		_ = writePurgeResult(t.Context(), &output, purgeErr)
		if strings.Contains(output.String(), "PRIVATE") {
			t.Fatalf("private error text leaked: %s", output.String())
		}
		stage := "machine_guard_" + step
		var fault purgeFault
		err = readPurgeResult(output.Bytes(), errors.New("child failed"))
		if !errors.As(err, &fault) || fault.Stage != stage || !strings.Contains(fault.Error(), stage) {
			t.Fatalf("%s: fault=%+v err=%v", step, fault, err)
		}
		projected := errorreport.ProjectFault(t.Context(), "paperboat-cli", "uninstall", "command", "command_failed", fault)
		if projected.Stage != stage || projected.Code != "purge_"+stage+"_failed" {
			t.Fatalf("%s: projected=%+v", step, projected)
		}
	}
}

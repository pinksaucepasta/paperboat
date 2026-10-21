package service

import (
	"bytes"
	"testing"
)

// The rollback restarts roles whose definition runs the replaced worker executable (see
// LifecycleConfig.RunsReplacedImage). On macOS the definition is a launchd property list: the worker
// roles must name the worker slot in ProgramArguments, and the carrier and session owner (pinned
// executables) must not, or a rollback would restart them or miss a stale updater.
func TestLaunchdDefinitionsNameTheReplacedExecutableOnlyForWorkerRoles(t *testing.T) {
	const worker = "/usr/local/libexec/paperboat/users/u501/bin/pb"
	const pinned = "/usr/local/libexec/paperboat/users/u501/releases/pins/abc/pb"
	for _, test := range []struct {
		kind       string
		executable string
		runsWorker bool
	}{
		{HostdKind, worker, true},
		{UpdaterKind, worker, true},
		{HostKind, worker, true},
		{CarrierKind, pinned, false},
		{SessionOwnerKind, pinned, false},
	} {
		definition, err := renderLaunchd(Config{Platform: "darwin", Kind: test.kind, Executable: test.executable, Arguments: []string{"daemon", "__runtime-" + test.kind}, User: "tester", Group: "staff", SessionOwnerGeneration: "g1"})
		if err != nil {
			t.Fatalf("%s: %v", test.kind, err)
		}
		if got := bytes.Contains(definition, []byte(worker)); got != test.runsWorker {
			t.Fatalf("%s: definition names the worker executable = %v, want %v\n%s", test.kind, got, test.runsWorker, definition)
		}
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile/atomicfiletest"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"github.com/spf13/cobra"
)

func freshResetTokenFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + string(os.PathSeparator) + "token"
	atomicfiletest.WriteOwnerAndSystemOnly(t, path, []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
	return path
}

// noRemember keeps a reset test from writing the replacement note of the real user.
func noRemember(t *testing.T) {
	t.Helper()
	original := rememberReplacedMachineFunc
	t.Cleanup(func() { rememberReplacedMachineFunc = original })
	rememberReplacedMachineFunc = func() (string, error) { return "", nil }
}

// noServer fails a test that reaches the server to replace a machine record.
func noServer(t *testing.T) {
	t.Helper()
	original := replaceUserMachine
	t.Cleanup(func() { replaceUserMachine = original })
	replaceUserMachine = func(*command.Context, string, string) error {
		t.Fatal("the old machine record was replaced before a new machine was verified")
		return nil
	}
}

// isolatedReplacementNote points the replacement note (and the user's state) at a temporary home.
func isolatedReplacementNote(t *testing.T) string {
	t.Helper()
	home := shorttemp.Dir(t) // macOS's local socket path must fit 104 bytes under the state root
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("APPDATA", filepath.Join(home, ".config")) // os.UserConfigDir on Windows
	return home
}

// F1-09: reset is make-before-break. It never removes the old machine record (the new pairing replaces it
// once it works); it notes the record before anything local is removed.
func TestFreshEnrollmentResetKeepsTheOldRecordAndNotesItBeforeCleanup(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	noServer(t)
	originalCleanup, originalRemember := freshEnrollmentCleanup, rememberReplacedMachineFunc
	t.Cleanup(func() { freshEnrollmentCleanup, rememberReplacedMachineFunc = originalCleanup, originalRemember })
	var order []string
	rememberReplacedMachineFunc = func() (string, error) { order = append(order, "remember"); return "machine_old", nil }
	freshEnrollmentCleanup = func(*cobra.Command) error { order = append(order, "cleanup"); return nil }
	var output bytes.Buffer
	command := freshEnrollmentResetCommand()
	command.SetOut(&output)
	command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", freshResetTokenFile(t), "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "remember,cleanup" || !strings.Contains(output.String(), `"replaces_machine_id":"machine_old"`) {
		t.Fatalf("order=%v output=%s", order, output.String())
	}
}

// F1-09 (soft-a-11051534): a reset that fails at any step names that step, never "could not finish the
// command", and leaves the old record (no server call) and the note for the next attempt.
func TestFreshEnrollmentResetFailureNamesTheStepAndKeepsTheOldRecord(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	noServer(t)
	originalCleanup, originalRemember := freshEnrollmentCleanup, rememberReplacedMachineFunc
	t.Cleanup(func() { freshEnrollmentCleanup, rememberReplacedMachineFunc = originalCleanup, originalRemember })
	for _, test := range []struct {
		name            string
		rememberErr     error
		cleanupErr      error
		want            string
		cleanupExpected bool
	}{
		{name: "note", rememberErr: os.ErrPermission, want: "nothing was removed"},
		{name: "cleanup", cleanupErr: freshResetCleanupError{err: uninstallCleanupError{err: uninstallStepFailure{name: "remove system Paperboat runtime", err: errors.New("exit status 1")}}}, want: "remove system Paperboat runtime", cleanupExpected: true},
	} {
		cleaned := false
		rememberReplacedMachineFunc = func() (string, error) { return "machine_old", test.rememberErr }
		freshEnrollmentCleanup = func(*cobra.Command) error { cleaned = true; return test.cleanupErr }
		command := freshEnrollmentResetCommand()
		command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", freshResetTokenFile(t), "--json"})
		err := command.Execute()
		value := classifyCLIJSONError(err)
		if err == nil || !strings.Contains(value.Message, test.want) || strings.Contains(value.Message, "could not finish the command") {
			t.Fatalf("%s: message=%q err=%v", test.name, value.Message, err)
		}
		if cleaned != test.cleanupExpected {
			t.Fatalf("%s: local cleanup ran=%v, want %v", test.name, cleaned, test.cleanupExpected)
		}
	}
	verify := classifyCLIJSONError(machineReplacementError{step: replaceStepVerifyCLI, cause: errors.New("digest changed")})
	if !strings.Contains(verify.Message, "old machine record and its address are kept") {
		t.Fatalf("verify step message=%q", verify.Message)
	}
}

// A machine that loses a replace race, or whose old record is in another scope, is told so in plain words
// instead of "check your connection"; other failures keep the generic text.
func TestMachineReplacementErrorNamesTheServerRefusal(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		want  string
		not   string
	}{
		{"already replaced", &api.APIError{Status: 409, Code: "user_machine_already_replaced"}, "Another machine already replaced machine_old. This machine is paired and working; see `pb machine list`.", "Check your connection"},
		{"team mismatch", fmt.Errorf("replace: %w", &api.APIError{Status: 409, Code: "user_machine_replace_team_mismatch"}), "belongs to a different team or account, so it was left as it is", "Check your connection"},
		{"other api error", &api.APIError{Status: 409, Code: "conflict"}, "Check your connection, then remove the old machine in the dashboard.", "Another machine"},
		{"network error", errors.New("dial tcp: timeout"), "Check your connection, then remove the old machine in the dashboard.", "Another machine"},
	} {
		got := classifyCLIJSONError(machineReplacementError{step: replaceStepCutover, retiredID: "machine_old", cause: test.cause}).Message
		if !strings.Contains(got, test.want) || strings.Contains(got, test.not) {
			t.Fatalf("%s: message=%q", test.name, got)
		}
	}
}

// F1-09: the note naming the old record and the local diagnostics survive the reset's wipe, so a pairing
// that fails afterwards can still be investigated and the next one still replaces the old record.
func TestFreshEnrollmentResetKeepsTheNoteAndDiagnosticsThroughTheWipe(t *testing.T) {
	isolatedReplacementNote(t)
	if err := bootstrap.SaveReplacement("machine_old", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	command := freshEnrollmentResetCommand()
	preserved := freshEnrollmentPreserved(command)
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := filepath.Join(paths.StateRoot, "diagnostics") // under XDG_STATE_HOME on Linux, Library on macOS
	note, _ := bootstrap.ReplacementPath()
	if len(preserved) != 2 || preserved[0] != note || preserved[1] != diagnostics {
		t.Fatalf("preserved=%v, want [%s %s]", preserved, note, diagnostics)
	}
	if err := os.MkdirAll(diagnostics, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diagnostics, "segment"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(filepath.Dir(note), "credentials")
	if err := os.WriteFile(credentials, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Dir(note), filepath.Dir(diagnostics)} {
		if err := removePathPreserving(root, preserved); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(credentials); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("other state survived the wipe: %v", err)
	}
	if bootstrap.LoadReplacement(time.Now()) != "machine_old" {
		t.Fatal("the replacement note was removed by the wipe")
	}
	if _, err := os.Lstat(filepath.Join(diagnostics, "segment")); err != nil {
		t.Fatalf("local diagnostics were removed by the wipe: %v", err)
	}
}

// F1-09/F1-10: the old record is replaced only after the new machine is verified, and the note is kept
// when the cutover fails, so nothing is stranded; with no note nothing is replaced.
func TestFinishMachineReplacementCutsOverOnlyAfterTheNewMachineWorks(t *testing.T) {
	isolatedReplacementNote(t)
	original := replaceUserMachine
	t.Cleanup(func() { replaceUserMachine = original })
	var calls []string
	var replaceErr error
	replaceUserMachine = func(_ *command.Context, replacement, retired string) error {
		calls = append(calls, replacement+"<"+retired)
		return replaceErr
	}
	ctx := &command.Context{Context: context.Background()}
	if err := finishMachineReplacement(ctx, "machine_new"); err != nil || len(calls) != 0 {
		t.Fatalf("no note: err=%v calls=%v", err, calls)
	}
	if err := bootstrap.SaveReplacement("machine_old", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A note that names only the machine that just paired is done with: nothing is replaced, and it is not
	// kept to mark that working machine as left behind later.
	if err := finishMachineReplacement(ctx, "machine_old"); err != nil || len(calls) != 0 || bootstrap.LoadReplacement(time.Now()) != "" {
		t.Fatalf("same record: err=%v calls=%v note=%q", err, calls, bootstrap.LoadReplacement(time.Now()))
	}
	if err := bootstrap.SaveReplacement("machine_old", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	replaceErr = errors.New("connection reset")
	err := finishMachineReplacement(ctx, "machine_new")
	value := classifyCLIJSONError(err)
	if err == nil || !strings.Contains(value.Message, "paired and working") || !strings.Contains(value.Message, "machine_old") {
		t.Fatalf("failed cutover: message=%q err=%v", value.Message, err)
	}
	if bootstrap.LoadReplacement(time.Now()) != "machine_old" {
		t.Fatal("the note was dropped although the cutover failed")
	}
	replaceErr = nil
	if err := finishMachineReplacement(ctx, "machine_new"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "machine_new<machine_old,machine_new<machine_old" || bootstrap.LoadReplacement(time.Now()) != "" {
		t.Fatalf("calls=%v note=%q", calls, bootstrap.LoadReplacement(time.Now()))
	}
}

// F1-09: a pairing that fails (anywhere up to the daemon bind) never replaces the old record.
func TestPairThatFailsNeverReplacesTheOldRecord(t *testing.T) {
	isolatedReplacementNote(t)
	noServer(t)
	if err := bootstrap.SaveReplacement("machine_old", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	original := pairBootstrap
	t.Cleanup(func() { pairBootstrap = original })
	reached := false
	pairBootstrap = func(context.Context, []string, io.Reader, io.Writer, io.Writer) int { reached = true; return 1 }
	command := pairCommand()
	command.PersistentFlags().String("server", "", "") // the root command's flag
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--server", "https://api.example.test", "--enrollment-token-file", freshResetTokenFile(t), "--state-root", t.TempDir()})
	if err := command.Execute(); err == nil || !reached {
		t.Fatalf("a failed pairing: err=%v, pairing reached=%v", err, reached)
	}
	if bootstrap.LoadReplacement(time.Now()) != "machine_old" {
		t.Fatal("a failed pairing dropped the note")
	}
}

func TestFreshEnrollmentResetRequiresCleanupBeforeSuccess(t *testing.T) {
	noRemember(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	original := freshEnrollmentCleanup
	t.Cleanup(func() { freshEnrollmentCleanup = original })
	cleanupCalls := 0
	freshEnrollmentCleanup = func(*cobra.Command) error {
		cleanupCalls++
		return errors.New("retained service")
	}
	command := freshEnrollmentResetCommand()
	command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", freshResetTokenFile(t), "--json"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "retained service") {
		t.Fatalf("reset error = %v", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d", cleanupCalls)
	}
}

func TestFreshEnrollmentResetReportsOnlyAfterCleanup(t *testing.T) {
	noRemember(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	original := freshEnrollmentCleanup
	t.Cleanup(func() { freshEnrollmentCleanup = original })
	freshEnrollmentCleanup = func(*cobra.Command) error { return nil }
	var output bytes.Buffer
	command := freshEnrollmentResetCommand()
	command.SetOut(&output)
	command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", freshResetTokenFile(t), "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ok":true`, `"reset":true`, `"inbox_preserved":true`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output %q missing %q", output.String(), want)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(executable)
	if !strings.Contains(output.String(), `"executable":`+string(encoded)) {
		t.Fatalf("reset did not report its retained executable: %s", output.String())
	}
	// macOS preserves the invocation link in os.Executable. Exercise the
	// public command shape as well as direct invocation without a source seam.
	if os.Getenv("PAPERBOAT_TEST_RESET_COMMAND_LINK") == "1" {
		return
	}
	link := filepath.Join(t.TempDir(), filepath.Base(executable))
	if err := os.Symlink(executable, link); err != nil {
		t.Logf("command link unavailable: %v", err)
		return
	}
	child := exec.Command(link, "-test.run=^TestFreshEnrollmentResetReportsOnlyAfterCleanup$", "-test.count=1")
	child.Env = append(os.Environ(), "PAPERBOAT_TEST_RESET_COMMAND_LINK=1")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("reset through command link failed: %v\n%s", err, output)
	}
}

func TestFreshEnrollmentCleanupRetainsStateAfterServiceFailure(t *testing.T) {
	statePurged := false
	runtimeAttempted := false
	err := performFreshEnrollmentCleanup(nil,
		func() error { return errors.New("daemon still running") },
		func() error { return nil },
		func() error { return nil },
		func() error { runtimeAttempted = true; return nil },
		func() error { return nil },
		func() error { statePurged = true; return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "stop local daemon") || !strings.Contains(err.Error(), "remove user Paperboat state") {
		t.Fatalf("cleanup error = %v", err)
	}
	if !runtimeAttempted {
		t.Fatal("independent runtime teardown was not attempted")
	}
	if statePurged {
		t.Fatal("credentials/state were deleted after unresolved daemon failure")
	}
}

// F1-09 (pbmac): a failed re-pair left an unfinished enrollment; reset's root
// purge failed, so the journal was retained and pb pair kept demanding pb reset.
func TestFreshEnrollmentResetDiscardsTheUnfinishedEnrollmentWhenTheRuntimePurgeFails(t *testing.T) {
	root := t.TempDir()
	record := bootstrap.NewResumeRecord("https://api.example.test", "public-key", "OLD-ENROLLMENT-TOKEN-0123456", "machine", "verifier-012345678901234567890123456789", time.Now().Add(time.Hour))
	if err := bootstrap.SaveResume(root, record); err != nil {
		t.Fatal(err)
	}
	command := freshEnrollmentResetCommand()
	if err := command.Flags().Set("state-root", root); err != nil {
		t.Fatal(err)
	}
	statePurged := false
	err := performFreshEnrollmentCleanup(nil,
		func() error { return nil },
		func() error { return nil },
		func() error { return nil },
		func() error { return errors.New("host runtime command failed") },
		func() error { return discardUnfinishedEnrollment(command) },
		func() error { statePurged = true; return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "remove system Paperboat runtime") || strings.Contains(err.Error(), "discard unfinished enrollment") {
		t.Fatalf("cleanup error = %v", err)
	}
	if statePurged {
		t.Fatal("credentials were deleted although the runtime purge failed")
	}
	if _, err := os.Lstat(bootstrap.ResumePath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unfinished enrollment survived reset: %v", err)
	}
	// The next pair with a new token starts fresh instead of asking for pb reset again.
	if matches, err := bootstrap.ResumeMatchesEnrollmentToken(root, "NEW-ENROLLMENT-TOKEN-0123456"); err != nil || matches {
		t.Fatalf("resume after reset: matches=%v err=%v", matches, err)
	}
	// A second reset over the already clean state is a no-op.
	if err := discardUnfinishedEnrollment(command); err != nil {
		t.Fatal(err)
	}
}

func TestFreshEnrollmentResetPreservesMatchingResume(t *testing.T) {
	noRemember(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	token := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	record := bootstrap.NewResumeRecord("https://api.example.test", "public-key", token, "machine", "verifier-012345678901234567890123456789", time.Now().Add(time.Hour))
	if err := bootstrap.SaveResume(root, record); err != nil {
		t.Fatal(err)
	}
	tokenFile := freshResetTokenFile(t)
	original := freshEnrollmentCleanup
	originalExecutable := resolveFreshEnrollmentExecutable
	t.Cleanup(func() {
		freshEnrollmentCleanup = original
		resolveFreshEnrollmentExecutable = originalExecutable
	})
	freshEnrollmentCleanup = func(*cobra.Command) error { t.Fatal("matching resume was reset"); return nil }
	resolveFreshEnrollmentExecutable = func() (string, error) { return "/owned/pb", nil }
	var output bytes.Buffer
	command := freshEnrollmentResetCommand()
	command.SetOut(&output)
	command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", tokenFile, "--state-root", root, "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"resume":true`, `"reset":false`, `"executable":"/owned/pb"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output %q missing %q", output.String(), want)
		}
	}
}

func TestIncompleteRemovalNamesFailedStagesAndWhatRemains(t *testing.T) {
	report := hostruntimecmd.PurgeIncomplete{Stages: []string{"machine_guard"}, Remaining: []string{"/Library/LaunchDaemons/io.paperboat.machineguard.plist"}}
	failure := uninstallStepFailure{name: "remove system Paperboat runtime", err: errors.Join(errors.New("exit status 1"), report)}
	for _, message := range []string{uninstallCleanupError{err: failure}.Error(), freshResetCleanupError{err: failure}.Error()} {
		for _, want := range []string{"remove system Paperboat runtime", "stages that failed: machine_guard", "Still present: /Library/LaunchDaemons/io.paperboat.machineguard.plist"} {
			if !strings.Contains(message, want) {
				t.Fatalf("%q lacks %q", message, want)
			}
		}
	}
}

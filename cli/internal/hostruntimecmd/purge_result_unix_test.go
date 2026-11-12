//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func TestPurgeResultRetainsAllStagesWithoutPrivatePayload(t *testing.T) {
	original := errors.Join(withPurgeStage("native_uninstall", &os.PathError{Op: "remove", Path: "PRIVATE_PATH_SENTINEL", Err: syscall.EACCES}), withPurgeStage("machine_guard", syscall.EIO), withPurgeStage("native_uninstall", nativePurgeFaultFixture{syscall.ENOSPC}))
	var output bytes.Buffer
	if err := writePurgeResult(context.Background(), &output, original); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("writer lost original error: %v", err)
	}
	if strings.Contains(output.String(), "PRIVATE_PATH_SENTINEL") || strings.Contains(output.String(), "permission denied") {
		t.Fatal("private error payload leaked into report")
	}
	childExit := errors.New("child failed")
	err := readPurgeResult(output.Bytes(), childExit)
	if !errors.Is(err, childExit) {
		t.Fatal("child exit failure was lost")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("stages not retained: %T", err)
	}
	faults := map[string]errorreport.Fault{}
	for _, cause := range joined.Unwrap() {
		if f, ok := cause.(purgeFault); ok {
			faults[f.Stage] = errorreport.ProjectFault(t.Context(), "paperboat-cli", "uninstall", "command", "command_failed", f)
		}
	}
	if len(faults) != 3 || faults["uninstall_power"].Errno != int(syscall.ENOSPC) || faults["uninstall_power"].Code != "purge_uninstall_power_failed" || faults["native_uninstall"].Errno != int(syscall.EACCES) || faults["machine_guard"].Errno != int(syscall.EIO) || faults["native_uninstall"].Code != "purge_native_uninstall_failed" {
		t.Fatalf("finite stage/errno lost: %+v", faults)
	}
}

func TestPurgeResultRejectsIncompleteOrContradictoryProof(t *testing.T) {
	var good bytes.Buffer
	if err := writePurgeResult(t.Context(), &good, nil); err != nil {
		t.Fatal(err)
	}
	var failure bytes.Buffer
	_ = writePurgeResult(t.Context(), &failure, withPurgeStage("account_sweep", syscall.EIO))
	for _, tc := range []struct {
		name string
		data []byte
		exit error
	}{
		{"missing", nil, nil},
		{"oversized", bytes.Repeat([]byte(" "), purgeResultLimit+1), nil},
		{"truncated", good.Bytes()[:8], nil},
		{"unknown_field", []byte(`{"version":"paperboat.purge-result/v1","ok":true,"private":"payload"}`), nil},
		{"trailing", append(append([]byte{}, good.Bytes()...), []byte(`{}`)...), nil},
		{"success_failed_exit", good.Bytes(), syscall.EIO},
		{"failure_success_exit", failure.Bytes(), nil},
		{"missing_ok", bytes.Replace(failure.Bytes(), []byte(`"ok":false,`), nil, 1), syscall.EIO},
		{"empty_failure", []byte(`{"version":"paperboat.purge-result/v1","ok":false}`), syscall.EIO},
		{"success_faults", bytes.Replace(failure.Bytes(), []byte(`"ok":false`), []byte(`"ok":true`), 1), nil},
		{"invalid_cause", bytes.Replace(failure.Bytes(), []byte(`"cause":"io"`), []byte(`"cause":"private_payload"`), 1), syscall.EIO},
		{"unclassified_stage", bytes.Replace(failure.Bytes(), []byte("account_sweep"), []byte("private_path"), -1), syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if readPurgeResult(tc.data, tc.exit) == nil {
				t.Fatal("invalid cleanup proof was accepted")
			}
		})
	}
	if err := readPurgeResult(good.Bytes(), nil); err != nil {
		t.Fatalf("complete success rejected: %v", err)
	}
	var bounded purgeOutput
	_, _ = bounded.Write(bytes.Repeat([]byte("x"), purgeResultLimit+200))
	_, _ = bounded.Write([]byte("more"))
	if !bounded.exceeded || len(bounded.data) != purgeResultLimit {
		t.Fatal("child output capture exceeded its memory bound")
	}
}

type purgeFailingWriter struct{}

func (purgeFailingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestPurgeResultWriteFailureCannotReportSuccess(t *testing.T) {
	if err := writePurgeResult(t.Context(), purgeFailingWriter{}, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write failure lost: %v", err)
	}
}

// Models the native owner's diagnostic error interface at the elevation protocol boundary.
type nativePurgeFaultFixture struct{ cause error }

func (f nativePurgeFaultFixture) Error() string           { return "private native failure payload" }
func (f nativePurgeFaultFixture) Unwrap() error           { return f.cause }
func (f nativePurgeFaultFixture) DiagnosticStage() string { return "uninstall_power" }
func (f nativePurgeFaultFixture) DiagnosticCode() string  { return "purge_uninstall_power_failed" }

// F1-09: a failed uninstall must say what is left, not only which step failed.
func TestPurgeResultCarriesWhatRemains(t *testing.T) {
	items := []string{"/Library/LaunchDaemons/io.paperboat.machineguard.plist", "process 412 (/Library/PrivilegedHelperTools/Paperboat/users/u501/bin/pb)"}
	var output bytes.Buffer
	_ = writePurgeResult(t.Context(), &output, errors.Join(withPurgeStage("machine_guard", syscall.EIO), withPurgeStage("final_inventory", unixLeftoverError{items: items})))
	var incomplete PurgeIncomplete
	if err := readPurgeResult(output.Bytes(), errors.New("exit 1")); !errors.As(err, &incomplete) {
		t.Fatalf("no cleanup report: %v", err)
	}
	if strings.Join(incomplete.Stages, ",") != "machine_guard,final_inventory" || strings.Join(incomplete.Remaining, "|") != strings.Join(items, "|") {
		t.Fatalf("report = %+v", incomplete)
	}
	for _, bad := range []string{`"relative/path"`, `"/a\u0007b"`, `""`} {
		forged := bytes.Replace(output.Bytes(), []byte(`"remaining":[`), []byte(`"remaining":[`+bad+`,`), 1)
		if readPurgeResult(forged, errors.New("exit 1")) == nil || errors.As(readPurgeResult(forged, errors.New("exit 1")), &incomplete) {
			t.Fatalf("forged remaining item %s accepted", bad)
		}
	}
	var good bytes.Buffer
	_ = writePurgeResult(t.Context(), &good, nil)
	success := bytes.Replace(good.Bytes(), []byte(`"ok":true`), []byte(`"ok":true,"remaining":["/x"]`), 1)
	if readPurgeResult(success, nil) == nil {
		t.Fatal("success with leftovers accepted")
	}
}

// What a cleanup removed by known names (after an unreadable lifecycle state) crosses the privileged boundary on
// success and is validated like the leftovers: absolute paths or processes only, bounded.
func TestPurgeResultCarriesWhatWasRemovedByName(t *testing.T) {
	var output bytes.Buffer
	removed := []string{"/etc/systemd/system/paperboat-hostd-u1001.service", "process 4001 (/usr/local/libexec/paperboat/users/u1001/bin/pb)"}
	if err := writePurgeResult(t.Context(), &output, nil, removed...); err != nil {
		t.Fatal(err)
	}
	got, err := readPurgeReport(output.Bytes(), nil)
	if err != nil || !slices.Equal(got, removed) {
		t.Fatalf("report=%v err=%v", got, err)
	}
	output.Reset()
	_ = writePurgeResult(t.Context(), &output, nil, "relative/path")
	if got, err := readPurgeReport(output.Bytes(), nil); err != nil || len(got) != 0 {
		t.Fatalf("an unsafe item crossed the boundary: %v %v", got, err)
	}
	forged := []byte(`{"version":"paperboat.purge-result/v1","ok":true,"removed_by_name":["relative/path"]}` + "\n")
	if _, err := readPurgeReport(forged, nil); err == nil {
		t.Fatal("a forged removal report was accepted")
	}
}

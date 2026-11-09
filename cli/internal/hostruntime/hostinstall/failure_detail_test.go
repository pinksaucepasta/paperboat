package hostinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// The Windows uninstall helper reported only {"cause":"internal"}; nobody could tell which step had failed.
func TestFailureDetailNamesEveryFailedStepWithoutPaths(t *testing.T) {
	denied := &fs.PathError{Op: "remove", Path: `C:\Program Files\Paperboat\users\u1\releases\pb.rollback.exe`, Err: syscall.EACCES}
	err := errors.Join(
		hostInstallFailure("remove Windows runtime services and slots", denied),
		hostInstallFailure("remove shared Paperboat directories and logs", ErrInvalidRequest),
		fmt.Errorf("guard uninstall: %w", errors.New("task is busy")),
		fmt.Errorf("state /var/lib/paperboat retained"),
		hostInstallFailure("remove Paperboat account payload", fmt.Errorf("walk %s: %w", `C:\ProgramData\Paperboat`, fs.ErrPermission)),
	)
	got := FailureDetail(err)
	want := "remove Windows runtime services and slots: remove: " + syscall.EACCES.Error() +
		"; remove shared Paperboat directories and logs: " + ErrInvalidRequest.Error() +
		"; guard uninstall: task is busy" +
		"; *errors.errorString" +
		"; remove Paperboat account payload: " + fs.ErrPermission.Error()
	if got != want {
		t.Fatalf("detail:\n got %q\nwant %q", got, want)
	}
	if strings.ContainsAny(got, `\/`) {
		t.Fatalf("detail holds a path: %q", got)
	}
}

func TestFailureDetailIsBoundedAndSurvivesBrokenErrors(t *testing.T) {
	if got := FailureDetail(nil); got != "" {
		t.Fatalf("nil error: %q", got)
	}
	var wide error
	for index := 0; index < 200; index++ {
		wide = errors.Join(wide, hostInstallFailure(fmt.Sprintf("step %03d %s", index, strings.Repeat("x", 40)), ErrInvalidRequest))
	}
	if got := FailureDetail(wide); len(got) == 0 || len(got) > MaxFailureDetail {
		t.Fatalf("detail length %d, want 1..%d", len(got), MaxFailureDetail)
	}
	if got := FailureDetail(panickingError{}); got != "" {
		t.Fatalf("broken error: %q", got)
	}
}

type panickingError struct{}

func (panickingError) Error() string { panic("broken") }

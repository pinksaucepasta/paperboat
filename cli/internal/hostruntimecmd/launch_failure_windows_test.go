//go:build windows

package hostruntimecmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A service that fails to start leaves the cause in its startup-error log, not only the error's type names.
func TestWindowsLaunchFailureLogNamesTheCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PaperboatUpdated-u1-startup-error.log")
	recordWindowsLaunchFailure(path, fmt.Errorf("validate updater installation binding: %w", errors.New("invalid Windows updater configuration")))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "\ncause: validate updater installation binding: invalid Windows updater configuration\n") {
		t.Fatalf("startup-error log lacks the cause: %s", body)
	}
}

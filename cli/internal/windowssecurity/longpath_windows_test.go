//go:build windows

package windowssecurity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLongPathExpandsAnEightDotThreeName(t *testing.T) {
	long := filepath.Join(t.TempDir(), "a directory with a long name")
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := windows.UTF16PtrFromString(long)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(encoded, &buffer[0], uint32(len(buffer)))
	if err != nil || n == 0 {
		t.Skipf("short names are disabled on this volume: %v", err)
	}
	short := windows.UTF16ToString(buffer[:n])
	if strings.EqualFold(short, long) {
		t.Skip("the volume produced no distinct short name")
	}
	if got := LongPath(short); !strings.EqualFold(got, LongPath(long)) {
		t.Fatalf("LongPath(%s) = %s, want %s", short, got, long)
	}
	missing := filepath.Join(long, "missing")
	if LongPath(missing) != missing {
		t.Fatal("a missing path changed")
	}
}

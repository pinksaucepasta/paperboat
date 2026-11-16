//go:build linux || darwin || windows

package machineguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"testing"
)

// Any change to the guard's code must bump Revision, or older installs keep the unfixed guard.
func TestGuardRevisionMatchesItsSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") && n != "revision.go" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(n + "\n"))
		h.Write(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))) // a CRLF checkout hashes the same
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != RevisionSourceSHA256 {
		t.Fatalf("the machine guard's code changed: bump Revision (now %d) and set RevisionSourceSHA256 = %q in revision.go", Revision, got)
	}
}

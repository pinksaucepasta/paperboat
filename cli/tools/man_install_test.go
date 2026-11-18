package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxInstallerRunsTheVerifiedProductAndRejectsAWrongDigest(t *testing.T) {
	skipOnWindows(t)
	payload := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PB_TEST_LOG\"\nif [ \"$1\" = install ] && [ \"$2\" = --install-dir ] && [ \"$4\" = --json ]; then mkdir -p \"$3\"; cp \"$0\" \"$3/pb\"; chmod 0755 \"$3/pb\"; printf '{\"ok\":true,\"data\":{\"executable\":\"%s\"}}\\n' \"$3/pb\"; fi\n"
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "wrong digest"}[valid], func(t *testing.T) {
			r := newFakeRelease(t, "Linux", "x86_64", "pb-linux-amd64", payload)
			if !valid {
				write(t, r.file("pb-linux-amd64"), payload+"# tampered\n", 0o755)
			}
			log, prefix := filepath.Join(r.root, "calls"), filepath.Join(r.root, "prefix")
			output, err := r.run(t, []string{"--no-setup", "--install-dir", filepath.Join(prefix, "bin")}, "PB_TEST_LOG="+log)
			calls, _ := os.ReadFile(log)
			if !valid {
				if err == nil || len(calls) != 0 || !strings.Contains(output, "product digest mismatch") {
					t.Fatalf("unverified payload executed: %v %s %s", err, calls, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("installer: %v %s", err, output)
			}
			if want := "install --install-dir " + filepath.Join(prefix, "bin") + " --json\n--version\n"; string(calls) != want {
				t.Fatalf("calls %q; want %q", calls, want)
			}
			if got := r.downloads(t); got[len(got)-1] != "https://github.com/"+fakeRepo+"/releases/download/"+fakeVersion+"/pb-linux-amd64" {
				t.Fatalf("downloads = %q", got)
			}
		})
	}
}

func TestPairWithoutTokenFailsBeforeAnyDownload(t *testing.T) {
	skipOnWindows(t)
	r := newFakeRelease(t, "Linux", "x86_64", "pb-linux-amd64", "#!/bin/sh\n")
	output, err := r.run(t, []string{"--pair"})
	if err == nil || !strings.Contains(output, "--pair requires") {
		t.Fatalf("error = %v, output = %s", err, output)
	}
	if len(r.downloads(t)) != 0 {
		t.Fatalf("download ran before missing-token rejection: %q", r.downloads(t))
	}
}

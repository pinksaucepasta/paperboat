//go:build windows && paperboat_native_e2e

package pty

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// F3-11 for a Windows client without a controller-side ssh hop in the input path: `pb connect` runs on the device in a local ConPTY (what a user at that console
// has), the shell on a peer floods with `yes`, a second viewer of the session never reads its output, a 256 KiB paste is typed and then Ctrl-C. The output the
// user sees must go quiet within 5 s and the shell must answer. It needs a paired device: PB_E2E_PB (its pb.exe) and PB_E2E_PEER (another machine of the account).
func TestDeviceClientCtrlCUnderFlood(t *testing.T) {
	pb, peer := os.Getenv("PB_E2E_PB"), os.Getenv("PB_E2E_PEER")
	if pb == "" || peer == "" {
		t.Skip("needs PB_E2E_PB (the pb.exe of a paired device) and PB_E2E_PEER (another machine of the account)")
	}
	name := fmt.Sprintf("smoke-n6-%d", time.Now().UnixNano()%1_000_000_000)
	first, firstOut := startNativePTY(t, pb, []string{"connect", peer, "--name", name, "--status-bar", "off"})
	defer first.CloseIO()
	firstOut.waitForWithin(t, "\x1b[?2004h", time.Minute)
	if _, err := first.Write([]byte("stty -echo; printf 'PB_REA%sY\\n' DY\r")); err != nil {
		t.Fatal(err)
	}
	firstOut.waitForWithin(t, "PB_READY", 30*time.Second)
	defer first.Write([]byte("exit\r"))

	listing, err := exec.Command(pb, "session", "list", peer, "--all", "--json").Output()
	if err != nil {
		t.Fatalf("session list: %v", err)
	}
	var catalog struct {
		Sessions []struct{ ID, Name string } `json:"sessions"`
		Data     struct {
			Sessions []struct{ ID, Name string } `json:"sessions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listing, &catalog); err != nil {
		t.Fatalf("session list output: %v", err)
	}
	identifier := ""
	for _, session := range append(catalog.Sessions, catalog.Data.Sessions...) {
		if session.Name == name {
			identifier = session.ID
		}
	}
	if identifier == "" {
		t.Fatalf("session %s not in the catalog", name)
	}
	adapter, root := nativeAdapter(t)
	stalled, err := adapter.Start(Command{Path: pb, Args: []string{"connect", peer, "--session", identifier, "--status-bar", "off"}, CWD: root, Dimensions: Dimensions{Columns: 80, Rows: 25}})
	if err != nil {
		t.Fatalf("second viewer: %v", err)
	}
	defer stalled.CloseIO() // its output is never read: the stalled viewer

	if _, err := first.Write([]byte("yes\r")); err != nil {
		t.Fatal(err)
	}
	firstOut.waitForWithin(t, "y\r\ny\r\ny", 30*time.Second)
	if _, err := first.Write([]byte(strings.Repeat(strings.Repeat("x", 1023)+"\n", 256))); err != nil {
		t.Fatal(err)
	}
	sent := time.Now()
	if _, err := first.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	last, lastChange := len(firstOut.String()), time.Now()
	for time.Since(lastChange) < time.Second {
		if time.Since(sent) > 60*time.Second {
			t.Fatalf("the output was still flowing 60 s after Ctrl-C")
		}
		time.Sleep(50 * time.Millisecond)
		if now := len(firstOut.String()); now != last {
			last, lastChange = now, time.Now()
		}
	}
	quiet := lastChange.Sub(sent)
	t.Logf("output quiet %.1f s after Ctrl-C", quiet.Seconds())
	if quiet > 5*time.Second {
		t.Fatalf("output stopped %.1f s after Ctrl-C, want under 5 s", quiet.Seconds())
	}
	if _, err := first.Write([]byte("printf 'PB_AFT%sR\\n' ER\r")); err != nil {
		t.Fatal(err)
	}
	firstOut.waitForWithin(t, "PB_AFTER", 15*time.Second)
}

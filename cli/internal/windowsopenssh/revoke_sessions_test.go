package windowsopenssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testEd25519Key(t *testing.T) string {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func TestSessionPIDsSelectsOnlyManagedSshdSessions(t *testing.T) {
	const service, stockService = 100, 900
	table := []processEntry{
		{service, 4, "pb.exe"},
		{101, service, "sshd.exe"},     // managed listener: kept
		{102, 101, "sshd-session.exe"}, // session
		{103, 101, "sshd-session.exe"},
		{104, 102, "powershell.exe"}, // shell of a session
		{105, 104, "conhost.exe"},
		{stockService, 4, "services.exe"},
		{901, stockService, "sshd.exe"}, // the machine's own sshd: never
		{902, 901, "sshd-session.exe"},
		{110, 4, "sshd.exe"}, // sshd not under a Paperboat service host
		{111, 110, "sshd-session.exe"},
		{120, service, "pb.exe"}, // other child of the service host: not under an sshd
		{121, 120, "cmd.exe"},
		{130, 131, "x.exe"}, // PID-reuse cycle
		{131, 130, "y.exe"},
	}
	got := sessionPIDs(table, map[uint32]bool{service: true})
	slices.Sort(got)
	if want := []uint32{102, 103, 104, 105}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected %v, want %v", got, want)
	}
	if got := sessionPIDs(table, map[uint32]bool{}); len(got) != 0 {
		t.Fatalf("no service host selected %v", got)
	}
}

func TestReconcileReportsOnlyARemovedKey(t *testing.T) {
	root := t.TempDir()
	a, b := testEd25519Key(t), testEd25519Key(t)
	for _, step := range []struct {
		keys           []string
		changed, taken bool
	}{{[]string{a, b}, true, false}, {[]string{a, b}, false, false}, {[]string{a}, true, true}, {[]string{a, b}, true, false}, {nil, true, true}} {
		changed, removed, err := ReconcileAuthorizedKeysRevoking(t.Context(), root, step.keys)
		if err != nil || changed != step.changed || removed != step.taken {
			t.Fatalf("keys %d: changed=%v removed=%v err=%v, want %v %v", len(step.keys), changed, removed, err, step.changed, step.taken)
		}
	}
}

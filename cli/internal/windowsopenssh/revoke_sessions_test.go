package windowsopenssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
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

func fingerprint(t *testing.T, line string) string {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return ssh.FingerprintSHA256(key)
}

func TestReconcileReturnsTheFingerprintsOfRemovedKeys(t *testing.T) {
	root := t.TempDir()
	a, b := testEd25519Key(t), testEd25519Key(t)
	for _, step := range []struct {
		keys    []string
		changed bool
		removed []string
	}{
		{[]string{a, b}, true, nil},
		{[]string{a, b}, false, nil},
		{[]string{a}, true, []string{fingerprint(t, b)}},
		{[]string{a, b}, true, nil},
		{nil, true, []string{fingerprint(t, a), fingerprint(t, b)}},
	} {
		changed, removed, err := ReconcileAuthorizedKeysRevoking(t.Context(), root, step.keys)
		slices.Sort(removed)
		slices.Sort(step.removed)
		if err != nil || changed != step.changed || !slices.Equal(removed, step.removed) {
			t.Fatalf("keys %d: changed=%v removed=%v err=%v, want %v %v", len(step.keys), changed, removed, err, step.changed, step.removed)
		}
	}
}

const (
	keyA = "SHA256:Gtk/8PpiffsCgWQypP3TLhbLIuONGXwCA+IEB60qjxc" // the fingerprint in a real service.log
	keyB = "SHA256:AbCdEfGhIjKlMnOpQrStUvWxYz0123456789+/AbCdE"
)

func TestParseSessionLogMapsTheChildPidToTheAcceptedKey(t *testing.T) {
	const real = `Connection from X port 49974 on X port 55136
Accepted key ED25519 ` + keyA + ` found at C:/ProgramData/Paperboat/users/u/ssh/authorized_keys/paperboat:33
user: X: Postponed publickey for X from X port 49974 ssh2 [preauth]
Accepted publickey for X from X port 49974 ssh2: ED25519 ` + keyA + `
User child is on pid 8572
user: X: Starting session: command for X from X port 49974 id 0 [postauth]
`
	got, err := parseSessionLog(strings.NewReader(real))
	if want := map[uint32]string{8572: keyA}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v err=%v want %v", got, err, want)
	}
	cases := map[string]string{
		"interleaved logins: neither pid is attributed":  "Accepted publickey for X from X port 1 ssh2: ED25519 " + keyA + "\nAccepted publickey for X from X port 3 ssh2: ED25519 " + keyB + "\nUser child is on pid 7\nUser child is on pid 8\n",
		"a pid line with no Accepted before it":          "User child is on pid 7\n",
		"a forged fingerprint is not a key":              "Accepted publickey for X from X port 1 ssh2: ED25519 SHA256:short\nUser child is on pid 7\n",
		"text before the line start is not a log line":   "user: Accepted publickey for X from X port 1 ssh2: ED25519 " + keyA + "\nUser child is on pid 7\n",
		"a pid reused by a newer unattributable session": "Accepted publickey for X from X port 1 ssh2: ED25519 " + keyA + "\nUser child is on pid 7\nAccepted publickey for X from X port 2 ssh2: ED25519 " + keyB + "\nAccepted publickey for X from X port 3 ssh2: ED25519 " + keyB + "\nUser child is on pid 7\n",
	}
	for name, log := range cases {
		if got, _ := parseSessionLog(strings.NewReader(log)); len(got) != 0 {
			t.Fatalf("%s: mapped %v", name, got)
		}
	}
	reused := "Accepted publickey for X from X port 1 ssh2: ED25519 " + keyA + "\nUser child is on pid 7\nAccepted publickey for X from X port 2 ssh2: ED25519 " + keyB + "\nUser child is on pid 7\n"
	if got, _ := parseSessionLog(strings.NewReader(reused)); !reflect.DeepEqual(got, map[uint32]string{7: keyB}) {
		t.Fatalf("a reused pid takes the newer key: %v", got)
	}
}

func TestSessionPIDsForKeysEndsOnlyTheRemovedKeysSessionsAndAnyUnnamedOne(t *testing.T) {
	const service = 100
	table := []processEntry{
		{service, 4, "pb.exe"}, {101, service, "sshd.exe"},
		{110, 101, "sshd-session.exe"}, {111, 110, "sshd-session.exe"}, {112, 111, "powershell.exe"}, // A's session
		{120, 101, "sshd-session.exe"}, {121, 120, "sshd-session.exe"}, {122, 121, "cmd.exe"}, // B's session
		{130, 101, "sshd-session.exe"}, {131, 130, "sshd-session.exe"}, // not in the log
		{900, 4, "sshd.exe"}, {901, 900, "sshd-session.exe"}, // stock sshd
	}
	services := map[uint32]bool{service: true}
	pidKey := map[uint32]string{111: keyA, 121: keyB}
	pids := func(removed map[string]bool, log map[uint32]string) []uint32 {
		got := sessionPIDsForKeys(table, services, log, removed)
		slices.Sort(got)
		return got
	}
	if got, want := pids(map[string]bool{keyA: true}, pidKey), []uint32{110, 111, 112, 130, 131}; !slices.Equal(got, want) {
		t.Fatalf("removed A: %v want %v (B's session must survive, the unnamed one ends)", got, want)
	}
	if got, want := pids(map[string]bool{keyB: true}, pidKey), []uint32{120, 121, 122, 130, 131}; !slices.Equal(got, want) {
		t.Fatalf("removed B: %v want %v", got, want)
	}
	if got, want := pids(nil, pidKey), []uint32{110, 111, 112, 120, 121, 122, 130, 131}; !slices.Equal(got, want) {
		t.Fatalf("unreadable log ends all: %v want %v", got, want)
	}
	if got := pids(map[string]bool{"SHA256:other": true}, pidKey); !slices.Equal(got, []uint32{130, 131}) {
		t.Fatalf("a key with no session ends only the unnamed one: %v", got)
	}
}

func TestAKeyThatCannotBeFingerprintedEndsAllSessions(t *testing.T) {
	root := t.TempDir()
	if _, _, err := ReconcileAuthorizedKeysRevoking(t.Context(), root, []string{testEd25519Key(t)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "authorized_keys", "paperboat")
	if err := os.WriteFile(path, []byte("not a key line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, removed, err := ReconcileAuthorizedKeysRevoking(t.Context(), root, nil); err != nil || !slices.Equal(removed, []string{AllSessions}) {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
}

func TestManagedGenerationMatchesTheWholeServiceName(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	for name, want := range map[string]bool{
		"PaperboatSshd-u-1-" + generation:                  true,
		"PaperboatSshd-u-10-" + generation:                 false, // another instance
		"PaperboatSshd-u-1-10-" + generation:               false,
		"PaperboatSshd-u-1-" + generation[1:]:              false,
		"PaperboatSshd-u-1-" + strings.ToUpper(generation): false,
		"PaperboatSshd-u-1":                                false,
		"Other-u-1-" + generation:                          false,
	} {
		if got, ok := managedGeneration(name, "u-1"); ok != want || ok && got != generation {
			t.Fatalf("%s: got (%q, %v), want %v", name, got, ok, want)
		}
	}
}

func TestUnreadableOrMissingLogFallsBackReportsItAndStillEndsTheRevokedSession(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.log")
	if err := os.WriteFile(good, []byte("Accepted publickey for X from X port 1 ssh2: ED25519 "+keyA+"\nUser child is on pid 111\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	longLine := filepath.Join(dir, "long.log")
	if err := os.WriteFile(longLine, []byte(strings.Repeat("x", 2<<20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	table := []processEntry{{100, 4, "pb.exe"}, {101, 100, "sshd.exe"}, {110, 101, "sshd-session.exe"}, {111, 110, "sshd-session.exe"}, {120, 101, "sshd-session.exe"}, {121, 120, "sshd-session.exe"}}
	for name, step := range map[string]struct {
		paths   []string
		removed []string
	}{
		"missing log":          {[]string{filepath.Join(dir, "absent.log"), good}, []string{keyA}},
		"log that is a folder": {[]string{dir}, []string{keyA}},
		"over-long line":       {[]string{longLine}, []string{keyA}},
		"unnamed removed key":  {[]string{good}, []string{AllSessions}},
		"no removed key":       {[]string{good}, nil},
	} {
		endKeys, pidKey, fallback := sessionKeysFromLogs(step.paths, step.removed)
		if fallback == "" || endKeys != nil {
			t.Fatalf("%s: fallback=%q endKeys=%v, want the reported end-all fallback", name, fallback, endKeys)
		}
		got := sessionPIDsForKeys(table, map[uint32]bool{100: true}, pidKey, endKeys)
		slices.Sort(got)
		if !slices.Equal(got, []uint32{110, 111, 120, 121}) { // the revoked session (110/111) ends, with every other
			t.Fatalf("%s: ended %v", name, got)
		}
	}
	endKeys, pidKey, fallback := sessionKeysFromLogs([]string{good}, []string{keyA})
	if fallback != "" || !endKeys[keyA] || pidKey[111] != keyA {
		t.Fatalf("a readable log must stay targeted: fallback=%q endKeys=%v pidKey=%v", fallback, endKeys, pidKey)
	}
}

func TestCappedLogKeepsTheTailOfWholeLinesAtTheCap(t *testing.T) {
	file, err := os.OpenFile(filepath.Join(t.TempDir(), "service.log"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	log, err := newCappedLog(file)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("a", 1023) + "\n"
	for i := 0; i < serviceLogCap/len(line)+10; i++ {
		if n, err := log.Write([]byte(line)); n != len(line) || err != nil {
			t.Fatalf("write %d: n=%d err=%v", i, n, err)
		}
	}
	if _, err := log.Write([]byte("last line\n")); err != nil {
		t.Fatal(err)
	}
	info, _ := file.Stat()
	if info.Size() > serviceLogCap || info.Size() < serviceLogKeep/2 {
		t.Fatalf("size %d after the cap %d", info.Size(), serviceLogCap)
	}
	body, _ := os.ReadFile(file.Name())
	if !strings.HasPrefix(string(body), strings.Repeat("a", 1023)+"\n") || !strings.HasSuffix(string(body), "last line\n") {
		t.Fatal("the kept tail does not start on a whole line and end on the last write")
	}
}

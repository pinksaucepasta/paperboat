package hostinstall

import (
	"reflect"
	"strings"
	"testing"
)

func sweepTestInstance(fill byte) string {
	return "u" + strings.Repeat(string(fill), 24)
}

func TestWindowsAccountServicesMatchOnlyThisAccount(t *testing.T) {
	instance := sweepTestInstance('a')
	other := sweepTestInstance('b')
	names := []string{
		"PaperboatHostd-" + instance,
		"PaperboatUpdated-" + instance,
		"PaperboatCarrier-" + instance + "-g1",
		"PaperboatSshd-" + instance,
		"PaperboatLocalDaemon-" + instance,
		"PaperboatUpdateActivator-" + instance,
		"PaperboatSessionOwner-" + instance + "-g1",
		"paperboathostd-" + strings.ToUpper(instance),
		"PaperboatUpdateActivator",
		"PaperboatHostd-" + other,
		"PaperboatCarrier-" + other + "-g1",
		"PaperboatSessionOwner-" + other + "-g1",
		"PaperboatSshd",
		"PaperboatHostd-" + instance + "x",
		"PaperboatLocalDaemon-" + instance + "-extra",
		"OtherVendorService-" + instance,
		"WinRM",
	}
	got := windowsAccountServices(names, instance)
	want := []string{
		"PaperboatHostd-" + instance,
		"PaperboatUpdated-" + instance,
		"PaperboatCarrier-" + instance + "-g1",
		"PaperboatSshd-" + instance,
		"PaperboatLocalDaemon-" + instance,
		"PaperboatUpdateActivator-" + instance,
		"PaperboatSessionOwner-" + instance + "-g1",
		"paperboathostd-" + strings.ToUpper(instance),
		"PaperboatUpdateActivator",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owned services = %q, want %q", got, want)
	}
}

func TestWindowsAccountServicesRejectEmptyInstance(t *testing.T) {
	if got := windowsAccountServices([]string{"PaperboatHostd-"}, ""); len(got) != 0 {
		t.Fatalf("empty instance matched %q", got)
	}
}

func TestWindowsImageUnderRootsUsesPathBoundaries(t *testing.T) {
	instance := sweepTestInstance('a')
	roots := []string{
		`C:\Program Files\Paperboat\users\` + instance,
		`C:\ProgramData\Paperboat\users\` + instance,
	}
	cases := []struct {
		image string
		want  bool
	}{
		{`C:\Program Files\Paperboat\users\` + instance + `\bin\pb.exe`, true},
		{`c:\programdata\paperboat\users\` + strings.ToUpper(instance) + `\hostd.exe`, true},
		{`C:\ProgramData\Paperboat\users\` + instance, true},
		{`C:\ProgramData\Paperboat\users\` + instance + `\`, true},
		{`C:\Program Files\Paperboat\users\` + instance + `2\bin\pb.exe`, false},
		{`C:\Program Files\Paperboat\users\` + sweepTestInstance('b') + `\bin\pb.exe`, false},
		{`C:\Program Files\Paperboat\MachineGuard\pb.exe`, false},
		{`C:\Users\pujan\AppData\Local\Temp\Paperboat Uninstall\x\pb.exe`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := windowsImageUnderRoots(tc.image, roots); got != tc.want {
			t.Errorf("windowsImageUnderRoots(%q) = %v, want %v", tc.image, got, tc.want)
		}
	}
	if windowsImageUnderRoots(`C:\anything`, []string{""}) {
		t.Fatal("an empty root matched an image")
	}
}

func TestWindowsOtherAccountEntriesKeepsOnlyForeignAccounts(t *testing.T) {
	instance := sweepTestInstance('a')
	got := windowsOtherAccountEntries([]string{instance, strings.ToUpper(instance), sweepTestInstance('b'), "junk"}, instance)
	want := []string{sweepTestInstance('b'), "junk"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("other accounts = %q, want %q", got, want)
	}
	if len(windowsOtherAccountEntries(nil, instance)) != 0 {
		t.Fatal("no entries must leave no other account")
	}
}

func TestWindowsPathListContainsExactEntries(t *testing.T) {
	bin := `C:\Program Files\Paperboat\users\ua\bin`
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"exact", `C:\Windows;` + bin + `;C:\Tools`, true},
		{"case and trailing separator", `c:\program files\paperboat\users\ua\BIN\`, true},
		{"prefix only", `C:\Program Files\Paperboat\users\ua\bin2;C:\Windows`, false},
		{"parent only", `C:\Program Files\Paperboat\users\ua;C:\Windows`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowsPathListContains(tc.value, bin); got != tc.want {
				t.Fatalf("windowsPathListContains = %v, want %v", got, tc.want)
			}
		})
	}
	if windowsPathListContains(bin, "") {
		t.Fatal("an empty directory matched")
	}
}

func TestWindowsAccountLeftoversNamesEveryItemKind(t *testing.T) {
	if got := windowsAccountLeftovers(nil, nil, nil, false); len(got) != 0 {
		t.Fatalf("clean account reported leftovers %q", got)
	}
	got := windowsAccountLeftovers(
		[]string{"PaperboatHostd-ua"},
		[]uint32{4242},
		[]string{"ProgramData instance directory"},
		true,
	)
	want := []string{"service PaperboatHostd-ua", "process 4242", "ProgramData instance directory", "machine PATH entry"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("leftovers = %q, want %q", got, want)
	}
}

// Security review, 2026-11-10: the sweep ended every process whose image lay under the account's roots, whoever
// ran it. Only the account's own processes are selected now.
func TestWindowsPayloadProcessesAreOnlyTheAccountsOwn(t *testing.T) {
	const owner, system, purger, stranger = "S-1-5-21-1-2-3-1001", "S-1-5-18", "S-1-5-21-1-2-3-500", "S-1-5-21-1-2-3-1002"
	root := `C:\Program Files\Paperboat\users\` + sweepTestInstance('a')
	type process struct {
		parent      uint32
		image, user string
	}
	processes := map[uint32]process{
		4:  {0, ``, system},                                // image cannot be read
		10: {4, root + `\bin\pb.exe`, system},              // the account's service
		11: {4, root + `\releases\pb.rollback.exe`, owner}, // an old CLI of the owner
		12: {4, root + `\bin\pb.exe`, strings.ToLower(purger)},
		13: {4, root + `\bin\pb.exe`, stranger},          // another user runs the account's image
		14: {4, root + `\bin\pb.exe`, ``},                // token cannot be read
		15: {4, `C:\Windows\System32\cmd.exe`, owner},    // the owner's, but not Paperboat
		20: {4, root + `\bin\pb.exe`, owner},             // the pb that asked for this purge
		21: {20, `C:\Temp\helper.exe`, owner},            // its elevated helper
		22: {21, root + `\bin\pb.exe`, owner},            // this process
		23: {22, root + `\bin\pb.exe`, owner},            // a child is not ancestry
		24: {0, root + `2\bin\pb.exe`, owner},            // a sibling root
		25: {4, strings.ToUpper(root) + `\X.EXE`, owner}, // case-insensitive
	}
	var entries []windowsProcessEntry
	for id := uint32(0); id < 30; id++ {
		if item, ok := processes[id]; ok {
			entries = append(entries, windowsProcessEntry{id: id, parent: item.parent})
		}
	}
	image := func(id uint32) (string, bool) { return processes[id].image, processes[id].image != "" }
	user := func(id uint32) (string, bool) { return processes[id].user, processes[id].user != "" }
	got := windowsPayloadProcessIDs(entries, []uint32{22, 21}, []string{root}, []string{owner, system, purger}, image, user)
	if want := []uint32{10, 11, 12, 23, 25}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected processes = %v, want %v", got, want)
	}
	// An account without a known owner still never matches a process whose user is unknown.
	if got := windowsPayloadProcessIDs(entries, nil, []string{root}, []string{""}, image, user); len(got) != 0 {
		t.Fatalf("an empty owner selected %v", got)
	}
}

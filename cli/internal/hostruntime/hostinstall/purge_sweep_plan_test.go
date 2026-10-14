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
		"PaperboatCarrier-" + instance,
		"PaperboatSshd-" + instance,
		"PaperboatLocalDaemon-" + instance,
		"PaperboatUpdateActivator-" + instance,
		"PaperboatSessionOwner-" + instance + "-g1",
		"paperboathostd-" + strings.ToUpper(instance),
		"PaperboatUpdateActivator",
		"PaperboatHostd-" + other,
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
		"PaperboatCarrier-" + instance,
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

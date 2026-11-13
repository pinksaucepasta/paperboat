package transfername

import (
	"strings"
	"testing"
)

func TestWindowsReceiverRefusesWhatWindowsCannotStore(t *testing.T) {
	for _, name := range []string{"a:b.txt", `q"u*?.txt`, "trail.", "sp ", "CON.txt", "con", "Aux.log", "NUL", "com1.txt", "LPT9", "a|b", "a<b", `a\b`, "a\x01b"} {
		if Refusal("windows", name) == "" {
			t.Errorf("windows accepted %q", name)
		}
	}
	for _, name := range []string{"A.txt", "a.txt", "-lead.txt", "café.txt", "sp ace.txt", "console.txt", "COM10.txt", "com.txt", strings.Repeat("x", 250) + ".txt"} {
		if reason := Refusal("windows", name); reason != "" {
			t.Errorf("windows refused %q: %s", name, reason)
		}
	}
}

func TestUnixReceiversOnlyRefuseWhatNoFileSystemStores(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		for _, name := range []string{"a:b.txt", "trail.", "CON.txt", `q"u*?.txt`, "-lead", "café", "sp ace"} {
			if reason := Refusal(platform, name); reason != "" {
				t.Errorf("%s refused %q: %s", platform, name, reason)
			}
		}
		for _, name := range []string{"", ".", "..", "a/b", "a\x00b", strings.Repeat("x", 256), "bad\xffutf8", "sp ", " lead", "a\\b", "a\nb"} {
			if Refusal(platform, name) == "" {
				t.Errorf("%s accepted %q", platform, name)
			}
		}
	}
}

func TestPortableNameStaysWithinTheFileSystemLimit(t *testing.T) {
	got := Portable("CON." + strings.Repeat("x", MaxBytes-4))
	if len(got) > MaxBytes || Refusal("windows", got) != "" || Portable("") != "" {
		t.Fatalf("Portable = %d bytes", len(got))
	}
}

func TestPortableNameIsSavedUnchangedOnEveryOS(t *testing.T) {
	for name, want := range map[string]string{
		"a:b.txt": "a_b.txt", `q"u*?.txt`: "q_u__.txt", "trail.": "trail_", "sp ": "sp_", "CON.txt": "_CON.txt", "com1": "_com1",
		"plain.txt": "plain.txt", "café-nfd.txt": "café-nfd.txt", "-lead.txt": "-lead.txt", "A.txt": "A.txt",
	} {
		got := Portable(name)
		if got != want {
			t.Fatalf("Portable(%q) = %q, want %q", name, got, want)
		}
		if reason := Refusal("windows", got); reason != "" {
			t.Fatalf("Portable(%q) = %q is still refused by Windows: %s", name, got, reason)
		}
	}
}

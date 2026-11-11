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
		for _, name := range []string{"a:b.txt", "trail.", "sp ", "CON.txt", `q"u*?.txt`, "-lead", "café"} {
			if reason := Refusal(platform, name); reason != "" {
				t.Errorf("%s refused %q: %s", platform, name, reason)
			}
		}
		for _, name := range []string{"", ".", "..", "a/b", "a\x00b", "a\nb", strings.Repeat("x", 256), "bad\xffutf8"} {
			if Refusal(platform, name) == "" {
				t.Errorf("%s accepted %q", platform, name)
			}
		}
	}
}

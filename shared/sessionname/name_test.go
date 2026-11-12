package sessionname

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, name := range []string{"build", "xd sp ünï LMW", "Zoë build 1", "東京 ビルド", "a.b:c", strings.Repeat("é", 64)} {
		if err := Validate(name); err != nil {
			t.Errorf("Validate(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "default", " lead", "trail ", "tab\there", "nul\x00", "esc\x1b[31m", "nl\nx", "bad\xffutf8", "zero​width", "sep x", strings.Repeat("é", 65)} {
		if err := Validate(name); err == nil {
			t.Errorf("Validate(%q) unexpectedly succeeded", name)
		}
	}
}

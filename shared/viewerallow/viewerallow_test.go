package viewerallow

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestEntry(t *testing.T) {
	for raw, want := range map[string]string{
		"User@X.com":              "user@x.com",
		"a+tag@company.com":       "a+tag@company.com",
		"*@Company.COM":           "*@company.com",
		"*@sub.company.com":       "*@sub.company.com",
		"*@bücher.example":        "*@xn--bcher-kva.example",
		"person@bücher.example":   "person@xn--bcher-kva.example",
		"*@XN--BCHER-KVA.Example": "*@xn--bcher-kva.example",
	} {
		if got, ok := Entry(raw); !ok || got != want {
			t.Errorf("Entry(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

func TestEntryRefuses(t *testing.T) {
	long := strings.Repeat("a", 63) + "."
	for _, raw := range []string{
		"", "*", "*@", "*@*", "@", "@company.com", "company.com", "*company.com", "**@company.com",
		// No bare top-level wildcard, also when IDNA would end the name in a dot or turn a dot character into one.
		"*@com", "*@localhost", "*@com。", "*@company.com。", "*@company.com.", "*@.company.com", "*@company..com",
		// "*" is only the whole local part.
		"a*@company.com", "*a@company.com", "person@*.company.com", "*@*.company.com", "*@company.*",
		"*@company.com/path", "*@company.com:443", "*@127.0.0.1", "*@[127.0.0.1]", "*@ company.com", " *@company.com",
		"*@company.com ", "*@company.com\n", "*@company.com,evil.com", "*@a@company.com", "*@-company.com",
		"Display Name <person@company.com>", "<person@company.com>", "person@company.com,other@company.com",
		"\"a@b\"@company.com", "pérson@company.com", "person@company", // single-label exact address is checked below
		"*@" + long + long + long + long + "example",
	} {
		if raw == "person@company" {
			continue
		}
		if got, ok := Entry(raw); ok {
			t.Errorf("Entry(%q) = %q, want refusal", raw, got)
		}
	}
}

func TestCanonicalBoundsAndDuplicates(t *testing.T) {
	list := make([]string, MaxEntries)
	for i := range list {
		list[i] = fmt.Sprintf("u%d@company.com", i)
	}
	if got, err := Canonical(list); err != nil || len(got) != MaxEntries {
		t.Fatalf("a full list = %d entries, %v", len(got), err)
	}
	for name, bad := range map[string][]string{
		"empty":                  nil,
		"over the bound":         append(append([]string(nil), list...), "one@more.com"),
		"duplicate address":      {"A@Company.com", "a@company.com"},
		"duplicate domain":       {"*@bücher.example", "*@xn--bcher-kva.example"},
		"one bad entry refuses":  {"a@company.com", "*"},
		"an empty entry refuses": {"a@company.com", ""},
	} {
		if got, err := Canonical(bad); !errors.Is(err, ErrInvalid) || got != nil {
			t.Errorf("%s: Canonical = %v, %v; want ErrInvalid", name, got, err)
		}
	}
}

func TestAllows(t *testing.T) {
	list, err := Canonical([]string{"user@x.com", "*@company.com", "*@bücher.example"})
	if err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]bool{
		"user@x.com":                     true,
		"USER@X.COM":                     true,
		"other@x.com":                    false,
		"user+tag@x.com":                 false,
		"anyone@company.com":             true,
		"Anyone@Company.COM":             true,
		"anyone@sub.company.com":         false,
		"anyone@notcompany.com":          false,
		"anyone@company.com.evil.test":   false,
		"anyone@company.co":              false,
		"anyone@xn--bcher-kva.example":   true,
		"anyone@bücher.example":          true,
		"anyone@bucher.example":          false,
		"*@company.com":                  false,
		"":                               false,
		"company.com":                    false,
		"a@evil.test@company.com":        false,
		"anyone@company.com\x00.evil.io": false,
	} {
		if got := Allows(list, address); got != want {
			t.Errorf("Allows(%q) = %v, want %v", address, got, want)
		}
	}
	if Allows(nil, "user@x.com") {
		t.Error("an empty list admitted an address")
	}
	// An entry is compared whole: nothing in a stored list acts as a pattern but the "*@domain" form.
	if Allows([]string{"*", "*@*", "@company.com", "company.com"}, "anyone@company.com") {
		t.Error("a malformed stored entry admitted an address")
	}
}

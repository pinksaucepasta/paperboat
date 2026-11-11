package main

import (
	"strings"
	"testing"
)

const sample = "# comment\nA=1\n\nKEY='-----BEGIN PRIVATE KEY-----\nAAAA\nBBBB\n-----END PRIVATE KEY-----'\nB=\"two words\"\nC=x=y\r\nLAST=z"

func TestParseEnvMultilineQuoted(t *testing.T) {
	entries, err := parseEnv(sample)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "KEY": "-----BEGIN PRIVATE KEY-----\nAAAA\nBBBB\n-----END PRIVATE KEY-----", "B": "two words", "C": "x=y", "LAST": "z"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %d, want %d", len(entries), len(want))
	}
	for _, e := range entries {
		if e.value != want[e.name] {
			t.Errorf("%s = %q, want %q", e.name, e.value, want[e.name])
		}
	}
}

func TestReplaceValueKeepsEveryOtherByte(t *testing.T) {
	entries, _ := parseEnv(sample)
	e, err := lookup(entries, "KEY")
	if err != nil {
		t.Fatal(err)
	}
	got, err := replaceValue(sample, e, "-----BEGIN PRIVATE KEY-----\nNEW\n-----END PRIVATE KEY-----")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sample, "AAAA\nBBBB", "NEW", 1)
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if again, _ := parseEnv(got); len(again) != len(entries) {
		t.Fatal("rewritten file parses differently")
	}
}

func TestParseEnvRejectsWhatItCannotInterpret(t *testing.T) {
	for name, text := range map[string]string{
		"unterminated":  "K='abc\nL=1",
		"trailing text": "K='abc' tail\n",
		"escape":        "K=\"a\\nb\"\n",
	} {
		if _, err := parseEnv(text); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestLookupNeedsExactlyOne(t *testing.T) {
	entries, _ := parseEnv("A=1\nA=2\n")
	if _, err := lookup(entries, "A"); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := lookup(entries, "B"); err == nil {
		t.Fatal("missing accepted")
	}
}

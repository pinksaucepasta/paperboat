package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestConvertDriveOperandsLeavesRemotesAndOptionsAlone(t *testing.T) {
	convert := func(value string) (string, error) { return "/c/" + strings.ReplaceAll(value[3:], `\`, "/"), nil }
	got, err := convertDriveOperands([]string{"-a", `C:\Users\me\tree`, "d:/x", "me@host:/c:/y", "box:path", "ab:\\z", `--exclude-from=C:\ex.txt`, "--bwlimit=5", "--rsh=ssh -o X=C:/y"}, convert)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-a", "/c/Users/me/tree", "/c/x", "me@host:/c:/y", "box:path", `ab:\z`, "--exclude-from=/c/ex.txt", "--bwlimit=5", "--rsh=ssh -o X=C:/y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRsyncRemoteShellChosen(t *testing.T) {
	for _, test := range []struct {
		args, environment []string
		want              bool
	}{
		{args: []string{"-a", "src/", "box:/tmp/x"}},
		{args: []string{"-e", "ssh", "src/", "box:/x"}, want: true},
		{args: []string{"-ave", "ssh", "src/", "box:/x"}, want: true},
		{args: []string{"--rsh=ssh -p 2", "src/", "box:/x"}, want: true},
		{args: []string{"--rsh", "ssh", "src/", "box:/x"}, want: true},
		{args: []string{"-fe", "src/", "box:/x"}},
		{args: []string{"--exclude=-e", "src/", "box:/x"}},
		{args: []string{"--", "-e", "box:/x"}},
		{args: []string{"-a", "src/", "box:/x"}, environment: []string{"Rsync_Rsh=ssh"}, want: true},
		{args: []string{"-Te", "src/", "box:/x"}},
		{args: []string{"-Me", "src/", "box:/x"}},
		{args: []string{"-avTe", "src/", "box:/x"}},
		{args: []string{"-T", "-e", "src/", "box:/x"}},
		{args: []string{"--temp-dir", "-e", "src/", "box:/x"}},
		{args: []string{"-a", "src/", "box:/x"}, environment: []string{"RSYNC_RSH="}},
	} {
		if got := rsyncRemoteShellChosen(test.args, test.environment); got != test.want {
			t.Errorf("%q %q: got %v", test.args, test.environment, got)
		}
	}
}

func TestRsyncRemoteShellArgumentAndDefaultEnvironment(t *testing.T) {
	if got := rsyncRemoteShellArgument("/c/Program Files/OpenSSH/ssh.exe"); got != "--rsh="+"'/c/Program Files/OpenSSH/ssh.exe'" {
		t.Fatalf("rsh = %q", got)
	}
	if got := rsyncRemoteShellArgument("/c/it's/ssh"); got != "--rsh="+"'/c/it''s/ssh'" {
		t.Fatalf("quoted rsh = %q", got)
	}
	got := withDefaultEnvironment([]string{"PATH=x"}, "MSYS2_ARG_CONV_EXCL", "*")
	if !reflect.DeepEqual(got, []string{"PATH=x", "MSYS2_ARG_CONV_EXCL=*"}) {
		t.Fatalf("added = %q", got)
	}
	kept := []string{"msys2_arg_conv_excl=/x"}
	if got := withDefaultEnvironment(kept, "MSYS2_ARG_CONV_EXCL", "*"); !reflect.DeepEqual(got, kept) {
		t.Fatalf("user value replaced: %q", got)
	}
}

func TestWithEnvironmentReplacesAnyCaseEntry(t *testing.T) {
	got := withEnvironment([]string{"PATH=x", "Home=C:\\msys64\\home\\u", "HOME=/home/u"}, "HOME", `C:\Users\u`)
	want := []string{"PATH=x", `HOME=C:\Users\u`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

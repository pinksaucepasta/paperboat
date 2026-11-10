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

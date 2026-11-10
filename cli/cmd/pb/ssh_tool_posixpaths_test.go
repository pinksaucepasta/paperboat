package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestConvertDriveOperandsLeavesRemotesAndOptionsAlone(t *testing.T) {
	convert := func(value string) (string, error) { return "/c/" + strings.ReplaceAll(value[3:], `\`, "/"), nil }
	got, err := convertDriveOperands([]string{"-a", `C:\Users\me\tree`, "d:/x", "me@host:/c:/y", "box:path", "ab:\\z"}, convert)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-a", "/c/Users/me/tree", "/c/x", "me@host:/c:/y", "box:path", `ab:\z`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

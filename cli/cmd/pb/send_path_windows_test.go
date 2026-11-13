//go:build windows

package main

import "testing"

func TestSendSourcePathKeepsTrailingSpaceOfExtendedLengthPath(t *testing.T) {
	const path = `\\?\C:\Users\x\sp `
	got, err := sendSourcePath(path)
	if err != nil || got != path {
		t.Fatalf("sendSourcePath(%q) = %q, %v; want the path unchanged", path, got, err)
	}
}

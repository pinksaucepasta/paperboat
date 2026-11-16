//go:build !windows

package main

import "testing"

// protectCredentialTestFile: the 0600 mode the tests write is already what the daemon's reader requires.
func protectCredentialTestFile(*testing.T, string) {}

//go:build !windows

package main

import "os"

func writeLoginEnrollmentToken(path, token string) error {
	return os.WriteFile(path, []byte(token), 0o600)
}

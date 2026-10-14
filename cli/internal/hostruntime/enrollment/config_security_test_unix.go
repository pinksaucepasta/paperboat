//go:build !windows

package enrollment

import "os"

func makeEnrollmentConfigPrivate(path string) error { return os.Chmod(path, 0o600) }

func makeEnrollmentStateRootPrivateForTest(string) error { return nil }

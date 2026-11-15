//go:build !windows

package shorttemp

import "testing"

func ownByUser(testing.TB, string) {}

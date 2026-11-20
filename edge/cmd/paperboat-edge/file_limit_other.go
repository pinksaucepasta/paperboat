//go:build !unix

package main

func warnIfFileLimitLow(int, int) bool { return true }

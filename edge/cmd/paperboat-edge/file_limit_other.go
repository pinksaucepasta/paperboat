//go:build !unix

package main

func warnIfFileLimitLow(int) bool { return true }

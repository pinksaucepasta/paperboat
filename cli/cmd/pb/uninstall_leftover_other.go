//go:build !windows

package main

func leftoverLockedBridgeDirectory(error) bool { return false }

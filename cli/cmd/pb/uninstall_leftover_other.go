//go:build !windows

package main

func lockedBridgeLeftover(error) (string, bool) { return "", false }

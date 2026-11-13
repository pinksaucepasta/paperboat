//go:build !windows

package server

func writeProtected(error) bool { return false }

//go:build !darwin && !linux

package main

import "os"

func openMCPSendSource(root *os.Root, path string) (*os.File, error) {
	return root.Open(path)
}

package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"
)

func TestInstallComponentFlags(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	for _, args := range [][]string{
		{"--enable", "relay"},
		{"--relay", "--edge"},
		{"--edge", "--both"},
	} {
		args = append([]string{"install", "--no-service", "--state-dir", dir}, args...)
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("pbh %v was accepted", args)
		}
	}
}

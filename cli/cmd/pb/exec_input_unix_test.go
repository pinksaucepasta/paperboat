//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

func TestExecNullInputIsEOFWithoutMutatingBorrowedFile(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var data [1]byte
	for range 2 {
		n, err := readExecInput(context.Background(), input, data[:])
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("null stdin read=%d err=%v; want EOF", n, err)
		}
	}
	if _, err := input.Stat(); err != nil {
		t.Fatalf("borrowed stdin was closed: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readExecInput(canceled, input, data[:]); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled null read=%v", err)
	}
}

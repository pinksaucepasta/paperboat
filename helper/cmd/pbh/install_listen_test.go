package main

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
)

func TestCheckClaimListenNamesAddressInUse(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	address := occupied.Addr().String()
	err = checkClaimListen(address)
	if err == nil || !strings.Contains(err.Error(), address) || !strings.Contains(err.Error(), "--claim-listen") {
		t.Fatalf("address in use error does not name the address and remedy: %v", err)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("address in use error lost its typed cause: %v", err)
	}
}

func TestCheckClaimListenAcceptsFreeAddress(t *testing.T) {
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := free.Addr().String()
	free.Close()
	if err := checkClaimListen(address); err != nil {
		t.Fatalf("free claim address refused: %v", err)
	}
}

func TestClaimListenErrorNamesAddressForOtherBindFailures(t *testing.T) {
	err := claimListenError("192.0.2.1:8443", errors.New("cannot assign requested address"))
	if err == nil || !strings.Contains(err.Error(), "192.0.2.1:8443") || !strings.Contains(err.Error(), "--claim-listen") {
		t.Fatalf("bind failure does not name the address and remedy: %v", err)
	}
}

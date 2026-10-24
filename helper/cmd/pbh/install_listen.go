package main

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// claimListenError names the claim address that could not be bound and the
// remedy, so the operator does not have to guess which port collided.
func claimListenError(address string, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("%w: %w", commandError(fmt.Sprintf("claim listen address %s is already in use by another service; choose a free address with --claim-listen HOST:PORT", address)), err)
	}
	return fmt.Errorf("%w: %w", commandError(fmt.Sprintf("claim listen address %s cannot be bound; check the address or choose another with --claim-listen HOST:PORT", address)), err)
}

// checkClaimListen proves the claim address is bindable before a fresh install
// writes any state, so a collision is refused instead of surfacing later as a
// service that never becomes ready.
func checkClaimListen(address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return claimListenError(address, err)
	}
	return listener.Close()
}

//go:build !linux

package main

import "os/exec"

func prepareConfigServiceQuery(*exec.Cmd) {}

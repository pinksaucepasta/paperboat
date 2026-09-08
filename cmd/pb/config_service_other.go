//go:build !windows

package main

import "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"

func windowsConfigServiceDefinition(string) (service.Config, bool, error) {
	return service.Config{}, false, nil
}

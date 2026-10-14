//go:build windows

package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWindowsSessionOwnerDeclarationBindsGeneration(t *testing.T) {
	instance := "u0123456789abcdef01234567"
	generation := strings.Repeat("a", 32)
	name, err := WindowsSessionOwnerName(instance, generation)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"daemon", "__runtime-session-owner", "--generation", generation, "--instance", instance}
	cfg := Config{Kind: SessionOwnerKind, Instance: instance, SessionOwnerGeneration: generation, Executable: `C:\Program Files\Paperboat\users\` + instance + `\releases\versions\2026.10.12.1\pb.exe`, User: "Paperboat", Arguments: args}
	body, err := renderWindowsService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var definition windowsServiceDefinition
	if err = json.Unmarshal(body, &definition); err != nil {
		t.Fatal(err)
	}
	if definition.Name != name || windowsServiceInstanceFromName(name) != instance || !windowsServiceUsesSID(name) || !windowsServiceArgumentsMatch(name, definition.Arguments) {
		t.Fatal("generation lost account identity, enrolled launcher, or command binding")
	}
	bad := append([]string(nil), args...)
	bad[3] = strings.Repeat("b", 32)
	if windowsServiceArgumentsMatch(name, bad) {
		t.Fatal("mismatched generation admitted")
	}
	if windowsServiceBaseNameFromName("PaperboatSessionOwner-"+instance+"-"+strings.Repeat("x", 32)) != "" {
		t.Fatal("invalid generation service admitted")
	}
}

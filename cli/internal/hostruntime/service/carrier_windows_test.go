//go:build windows

package service

import (
	"encoding/json"
	"testing"
)

func TestWindowsCarrierDeclarationBindsAccount(t *testing.T) {
	instance := "u0123456789abcdef01234567"
	name, err := WindowsCarrierName(instance)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Platform: "windows", Kind: CarrierKind, Instance: instance, Executable: `C:\Program Files\Paperboat\users\` + instance + `\releases\versions\2026.10.12.1\pb.exe`, User: "Paperboat", Arguments: []string{"daemon", "__runtime-carrier", "--instance", instance}}
	body, err := renderWindowsService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var definition windowsServiceDefinition
	if err := json.Unmarshal(body, &definition); err != nil {
		t.Fatal(err)
	}
	if definition.Name != name || !windowsServiceUsesSID(name) || !windowsServiceArgumentsMatch(name, definition.Arguments) {
		t.Fatal("carrier lost SCM account or exact command custody")
	}
	foreign := append([]string(nil), cfg.Arguments...)
	foreign[3] = "u1123456789abcdef01234567"
	if windowsServiceArgumentsMatch(name, foreign) || windowsServiceBaseNameFromName(name+"-extra") != "" {
		t.Fatal("foreign account or second carrier generation admitted")
	}
}

//go:build windows

package service

import (
	"encoding/json"
	"testing"
)

func TestWindowsCarrierDeclarationBindsAccount(t *testing.T) {
	instance := "u0123456789abcdef01234567"
	generation := "0123456789abcdef0123456789abcdef"
	name, err := WindowsCarrierName(instance, generation)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Platform: "windows", Kind: CarrierKind, Instance: instance, Generation: generation, Executable: `C:\Program Files\Paperboat\users\` + instance + `\releases\versions\2026.10.12.1\pb.exe`, User: "Paperboat", Arguments: []string{"daemon", "__runtime-carrier", "--instance", instance, "--generation", generation}}
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
	other := append([]string(nil), cfg.Arguments...)
	other[5] = "fedcba9876543210fedcba9876543210"
	if windowsServiceArgumentsMatch(name, foreign) || windowsServiceArgumentsMatch(name, other) || windowsServiceBaseNameFromName(name+"-extra") != "" || windowsServiceBaseNameFromName("PaperboatCarrier-"+instance) != "" {
		t.Fatal("foreign account, another generation's command or a malformed carrier name admitted")
	}
}

package service

import (
	"path/filepath"
	"strings"
	"testing"
)

// Windows carriers run one service per generation, like session owners, so an update's standby candidate runs
// beside the serving one (W21 Part A).
func TestWindowsCarrierGenerationNames(t *testing.T) {
	instance, generation := "u0123456789abcdef01234567", "0123456789abcdef0123456789abcdef"
	name, err := WindowsCarrierName(instance, generation)
	if err != nil || name != "PaperboatCarrier-"+instance+"-"+generation {
		t.Fatalf("name %q err %v", name, err)
	}
	if gotInstance, gotGeneration, valid := windowsCarrierNameParts(name); !valid || gotInstance != instance || gotGeneration != generation {
		t.Fatalf("parts %q %q %v", gotInstance, gotGeneration, valid)
	}
	for _, bad := range []string{"PaperboatCarrier-" + instance, "PaperboatCarrier-" + instance + "-" + generation + "x", "PaperboatCarrier-u1-" + generation, "PaperboatSessionOwner-" + instance + "-" + generation} {
		if _, _, valid := windowsCarrierNameParts(bad); valid {
			t.Fatalf("%q accepted as a carrier generation", bad)
		}
	}
	if _, err := WindowsCarrierName(instance, ""); err == nil {
		t.Fatal("a carrier without a generation was named")
	}
}

// winpool-4 (Part A device run): pairing failed twice with "invalid service definition": the Windows definition
// path check still expected one carrier per account, and a Windows carrier was not a generational unit. A carrier generation's installer is built like a session
// owner's and declares PaperboatCarrier-<instance>-<generation>.
func TestWindowsCarrierGenerationInstallerPath(t *testing.T) {
	instance, generation := "u0123456789abcdef01234567", "0123456789abcdef0123456789abcdef"
	installer, err := NewPending(Config{Platform: "windows", Kind: CarrierKind, Instance: instance, Generation: generation, ConfigRoot: t.TempDir(), Executable: filepath.Join(t.TempDir(), "pb.exe"), User: "Paperboat", Group: "Paperboat",
		Arguments: []string{"daemon", "__runtime-carrier", "--instance", instance, "--generation", generation}, Controller: WindowsController{}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "PaperboatCarrier-" + instance + "-" + generation + ".json"; !strings.HasSuffix(installer.DefinitionPath(), want) {
		t.Fatalf("definition %q, want one named %q", installer.DefinitionPath(), want)
	}
}

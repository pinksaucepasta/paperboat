package teams

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The CLI cannot import the server module, so it keeps its own list of machine capabilities. This
// test reads that list from the CLI source: a capability the server accepts but the CLI refuses
// (private_access once was) makes a feature unusable from the product.
func TestCLIOffersExactlyTheServersMachineCapabilities(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "cli", "cmd", "pb", "team_machine_command.go"))
	if err != nil {
		t.Skipf("CLI source is not part of this checkout: %v", err)
	}
	match := regexp.MustCompile(`var teamMachineCapabilities = \[\]string\{([^}]*)\}`).FindSubmatch(source)
	if match == nil {
		t.Fatal("cli/cmd/pb/team_machine_command.go no longer declares teamMachineCapabilities")
	}
	var cli []string
	for _, quoted := range strings.Split(string(match[1]), ",") {
		if name := strings.Trim(strings.TrimSpace(quoted), `"`); name != "" {
			cli = append(cli, name)
		}
	}
	server := slices.Clone(MachineCapabilities)
	slices.Sort(cli)
	slices.Sort(server)
	if !slices.Equal(cli, server) {
		t.Fatalf("CLI grants %v but the server accepts %v", cli, server)
	}
	for _, capability := range server {
		if !ValidMachineCapability(capability) {
			t.Fatalf("%s is listed but not valid", capability)
		}
	}
	if ValidMachineCapability("not_a_capability") {
		t.Fatal("an unknown capability is valid")
	}
}

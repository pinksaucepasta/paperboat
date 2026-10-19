//go:build linux || darwin || windows

package machineguard

import (
	"errors"
	"testing"
)

func TestGuardWithoutRecordedDomainServesNoBrowserAccess(t *testing.T) {
	state := t.TempDir()
	allowUnprivilegedDomainRecord(t)
	if _, err := selectedBrowserDomain(state, "1000"); !errors.Is(err, ErrBrowserDomainUnset) {
		t.Fatalf("selected domain without a recorded default: %v", err)
	}
	status := (&guardServer{cfg: Config{StateDir: state}, trustReady: true}).runtimeStatus("1000")
	if status.BrowserDomain != "" || status.DefaultBrowserDomain != "" || status.TrustReady {
		t.Fatalf("status without a domain = %+v", status)
	}
	if validName("hp."+testBrowserDomain, "") {
		t.Fatal("a machine name was valid without a domain")
	}
}

func TestRecordedDefaultDomainIsUsedUnlessOwnerChoseAnother(t *testing.T) {
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	domains, err := loadBrowserDomains(state)
	if err != nil || domains.Default != testBrowserDomain {
		t.Fatalf("recorded default = %+v, %v", domains, err)
	}
	if got, err := domains.selected("1000"); err != nil || got != testBrowserDomain {
		t.Fatalf("owner without a choice uses %q, %v", got, err)
	}
	domains.Owners["1001"] = "mynet.xyz"
	if got, _ := domains.selected("1001"); got != "mynet.xyz" {
		t.Fatalf("owner choice = %q", got)
	}
	approved := domains.approved()
	if len(approved) != 2 || !approved[testBrowserDomain] || !approved["mynet.xyz"] {
		t.Fatalf("approved = %v", approved)
	}
	if domains.renewalJournal(testBrowserDomain) != "local-ca-renewal.pem" || domains.renewalJournal("mynet.xyz") == "local-ca-renewal.pem" {
		t.Fatal("renewal journals are not keyed by the default domain")
	}
	status := (&guardServer{cfg: Config{StateDir: state}, trustReady: true}).runtimeStatus("1000")
	if status.BrowserDomain != testBrowserDomain || status.DefaultBrowserDomain != testBrowserDomain {
		t.Fatalf("status = %+v", status)
	}
}

func TestRecordingTheDefaultDomainKeepsOrReplacesIt(t *testing.T) {
	state := t.TempDir()
	recordTestDefaultDomain(t, state)
	if err := recordDefaultBrowserDomain(state, ""); err != nil {
		t.Fatal(err)
	}
	if domains, _ := loadBrowserDomains(state); domains.Default != testBrowserDomain {
		t.Fatalf("an empty domain replaced the recorded one: %q", domains.Default)
	}
	// An owner who had chosen the new deployment domain simply follows the default.
	domains, _ := loadBrowserDomains(state)
	domains.Owners["1000"] = "other.example.test"
	if err := writeBrowserDomains(state, domains); err != nil {
		t.Fatal(err)
	}
	if err := recordDefaultBrowserDomain(state, "other.example.test"); err != nil {
		t.Fatal(err)
	}
	domains, err := loadBrowserDomains(state)
	if err != nil || domains.Default != "other.example.test" || len(domains.Owners) != 0 {
		t.Fatalf("replaced default = %+v, %v", domains, err)
	}
	if err := recordDefaultBrowserDomain(state, "com"); err == nil {
		t.Fatal("a public suffix was recorded as the deployment domain")
	}
}

func TestMachineNamesAreScopedToTheOwnersDomain(t *testing.T) {
	for _, name := range []string{"hp." + testBrowserDomain, "studio-1." + testBrowserDomain} {
		if !validName(name, testBrowserDomain) {
			t.Fatalf("rejected %s", name)
		}
		if validName(name, "mynet.xyz") {
			t.Fatalf("accepted %s in another owner's domain", name)
		}
	}
	if !validSubdomainName("3000.hp.mynet.xyz", "mynet.xyz") || validSubdomainName("3000.hp.mynet.xyz", testBrowserDomain) {
		t.Fatal("port names must follow the owner's domain")
	}
}

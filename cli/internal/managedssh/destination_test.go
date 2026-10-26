package managedssh

import (
	"errors"
	"testing"
)

func TestAliasHostRoundTrip(t *testing.T) {
	host, err := AliasHost("Build-01", testAliasDomain)
	if err != nil || host != "build-01.machines.example.test" {
		t.Fatalf("AliasHost() = %q, %v", host, err)
	}
	alias, err := ParseAliasHost(host, testAliasDomain)
	if err != nil || alias != "build-01" {
		t.Fatalf("ParseAliasHost() = %q, %v", alias, err)
	}
}

func TestParseMachineTarget(t *testing.T) {
	for input, want := range map[string][2]string{
		"build-01":            {"build-01", ""},
		"root@build-01":       {"build-01", "root"},
		"Deploy@BUILD-01":     {"BUILD-01", "Deploy"},
		"root@machine_01":     {"machine_01", "root"},
		`VICTUS\Pujan@victus`: {"victus", `VICTUS\Pujan`},
	} {
		alias, username, err := ParseMachineTarget(input)
		if err != nil || alias != want[0] || username != want[1] {
			t.Fatalf("ParseMachineTarget(%q) = %q, %q, %v", input, alias, username, err)
		}
	}
	for _, input := range []string{"", "@build", "root@", "root@user@build", "-root@build", "root@bad\nmachine"} {
		if _, _, err := ParseMachineTarget(input); err == nil {
			t.Fatalf("ParseMachineTarget(%q) succeeded", input)
		}
	}
}

func TestAliasHostRejectsNonCanonicalDestinations(t *testing.T) {
	for _, domain := range []string{"", "example", "*.example.test", "Example.Test", "example.test/path"} {
		if _, err := AliasHost("build", domain); !errors.Is(err, ErrSSHAliasInvalid) {
			t.Fatalf("invalid or missing domain %q admitted: %v", domain, err)
		}
	}
	for _, host := range []string{"machines.example.test", "a.b.machines.example.test", "-bad.machines.example.test", "bad-.machines.example.test", "machine.invalid", "machine.example.test", "bad.other.example.test", "127.0.0.1"} {
		if _, err := ParseAliasHost(host, testAliasDomain); !errors.Is(err, ErrSSHAliasInvalid) {
			t.Fatalf("ParseAliasHost(%q, testAliasDomain) error = %v", host, err)
		}
	}
	for _, alias := range []string{"", "-bad", "bad-", "bad.name", "bad/name"} {
		if _, err := AliasHost(alias, testAliasDomain); !errors.Is(err, ErrSSHAliasInvalid) {
			t.Fatalf("AliasHost(%q, testAliasDomain) error = %v", alias, err)
		}
	}
}

func TestResolveUsernamePrecedence(t *testing.T) {
	tests := []struct {
		name          string
		requested     string
		openSSH       string
		registered    string
		local         string
		hasRegistered bool
		want          string
	}{
		{name: "requested registered", requested: "hosted", openSSH: "hosted", registered: "hosted", local: "local", hasRegistered: true, want: "hosted"},
		{name: "openssh registered", openSSH: "hosted", registered: "hosted", local: "local", hasRegistered: true, want: "hosted"},
		{name: "registered", registered: "hosted", local: "local", hasRegistered: true, want: "hosted"},
		{name: "local only without registered user", local: "local", want: "local"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveUsername(test.requested, test.openSSH, test.registered, test.local, test.hasRegistered)
			if err != nil || got != test.want {
				t.Fatalf("ResolveUsername() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestResolveUsernameRejectsConflictAndUnsafeValues(t *testing.T) {
	if _, err := ResolveUsername("root", "deploy", "", "", false); !errors.Is(err, ErrSSHUsernameConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := ResolveUsername("", "", "", "local", true); !errors.Is(err, ErrSSHUsernameMissing) {
		t.Fatalf("registered-user absence error = %v", err)
	}
	// Another OS user than the one who set pb up is not refused: they sign in with their own credentials.
	if got, err := ResolveUsername("bob", "", "deploy", "local", true); err != nil || got != "bob" {
		t.Fatalf("another user = %q, %v", got, err)
	}
	for _, value := range []string{"-oProxyCommand=x", "user@host", "bad user", "bad;user", "bad\nuser", ""} {
		_, err := ResolveUsername(value, "", "", "", false)
		if value == "" {
			if !errors.Is(err, ErrSSHUsernameMissing) {
				t.Fatalf("ResolveUsername(%q) error = %v", value, err)
			}
		} else if !errors.Is(err, ErrSSHUsernameInvalid) {
			t.Fatalf("ResolveUsername(%q) error = %v", value, err)
		}
	}
}

func TestResolveUsernameWindowsIsCaseInsensitiveAndCanonical(t *testing.T) {
	for _, request := range []string{"", `victus\pujan`} {
		got, err := ResolveUsernameForPlatform(request, "", `VICTUS\Pujan`, "local", true, "windows")
		if err != nil || got != `VICTUS\Pujan` {
			t.Fatalf("qualified Windows identity = %q, %v", got, err)
		}
	}
	if _, err := ResolveUsernameForPlatform(`VICTUS\Pujan`, "", "", "", false, "linux"); !errors.Is(err, ErrSSHUsernameInvalid) {
		t.Fatalf("Unix accepted qualified Windows identity: %v", err)
	}
	for _, value := range []string{`\Pujan`, `VICTUS\`, `VICTUS\Pujan\extra`, `-domain\Pujan`, `VICTUS\-user`, `VICTUS/Pujan`, `VICTUS\bad user`, `VICTUS\bad;user`} {
		if _, err := ResolveUsernameForPlatform(value, "", "", "", false, "windows"); !errors.Is(err, ErrSSHUsernameInvalid) {
			t.Fatalf("unsafe qualified identity %q accepted: %v", value, err)
		}
	}
	got, err := ResolveUsernameForPlatform("pujan", "Pujan", "Pujan", "local", true, "windows")
	if err != nil || got != "Pujan" {
		t.Fatalf("ResolveUsernameForPlatform() = %q, %v", got, err)
	}
	if got, err := ResolveUsernameForPlatform("pujan", "", "Pujan", "local", true, "linux"); err != nil || got != "pujan" {
		t.Fatalf("Linux user names are case-sensitive, so this is another user: %q, %v", got, err)
	}
}

func TestResolveDestinationFencesPort(t *testing.T) {
	input := DestinationInput{AliasDomain: testAliasDomain, Alias: "build", RegisteredPort: 2222, RequestedPort: 2222, RegisteredUser: "deploy", HasRegisteredUser: true}
	got, err := ResolveDestination(input)
	if err != nil || got.Host != "build.machines.example.test" || got.Port != 2222 || got.User != "deploy" {
		t.Fatalf("ResolveDestination() = %#v, %v", got, err)
	}
	input.RequestedPort = 22
	if _, err := ResolveDestination(input); !errors.Is(err, ErrSSHPortConflict) {
		t.Fatalf("port conflict error = %v", err)
	}
	if _, err := ValidateDestinationPort("2222", 2222); err != nil {
		t.Fatalf("ValidateDestinationPort() error = %v", err)
	}
	if _, err := ValidateDestinationPort("22", 2222); !errors.Is(err, ErrSSHPortConflict) {
		t.Fatalf("ValidateDestinationPort conflict error = %v", err)
	}
}

// Only the OS user who set pb up on the machine is the managed user (gets the automatic key).
func TestResolveDestinationMarksOnlyTheSetupUserAsManaged(t *testing.T) {
	input := DestinationInput{AliasDomain: testAliasDomain, Alias: "homelab", RegisteredPort: 22, RegisteredUser: "pbt", HasRegisteredUser: true}
	for requested, managed := range map[string]bool{"": true, "pbt": true, "bob": false, "PBT": false} {
		input.RequestedUser = requested
		got, err := ResolveDestination(input)
		if err != nil || got.Managed != managed {
			t.Fatalf("user %q: managed=%v err=%v, want managed=%v", requested, got.Managed, err, managed)
		}
	}
	input.RequestedUser, input.Platform, input.RegisteredUser = "pujan", "windows", "Pujan"
	if got, err := ResolveDestination(input); err != nil || !got.Managed || got.User != "Pujan" {
		t.Fatalf("Windows user names are case-insensitive: %+v %v", got, err)
	}
}

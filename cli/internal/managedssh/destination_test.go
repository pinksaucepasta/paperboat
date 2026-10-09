package managedssh

import (
	"errors"
	"testing"
)

func TestAliasHostRoundTrip(t *testing.T) {
	host, err := AliasHost("Build-01")
	if err != nil || host != "build-01.local.pprbt.dev" {
		t.Fatalf("AliasHost() = %q, %v", host, err)
	}
	alias, err := ParseAliasHost(host)
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
	for _, host := range []string{"local.pprbt.dev", "a.b.local.pprbt.dev", "-bad.local.pprbt.dev", "bad-.local.pprbt.dev", "machine.pprbt", "machine.pprbt.dev", "bad.other.dev", "127.0.0.1"} {
		if _, err := ParseAliasHost(host); !errors.Is(err, ErrSSHAliasInvalid) {
			t.Fatalf("ParseAliasHost(%q) error = %v", host, err)
		}
	}
	for _, alias := range []string{"", "-bad", "bad-", "bad.name", "bad/name"} {
		if _, err := AliasHost(alias); !errors.Is(err, ErrSSHAliasInvalid) {
			t.Fatalf("AliasHost(%q) error = %v", alias, err)
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
	if got, err := ResolveUsername("root", "", "deploy", "local", true); err != nil || got != "root" {
		t.Fatalf("explicit user override = %q, %v", got, err)
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
		t.Fatalf("Linux explicit override = %q, %v", got, err)
	}
}

func TestResolveDestinationFencesPort(t *testing.T) {
	input := DestinationInput{Alias: "build", RegisteredPort: 2222, RequestedPort: 2222, RegisteredUser: "deploy", HasRegisteredUser: true}
	got, err := ResolveDestination(input)
	if err != nil || got.Host != "build.local.pprbt.dev" || got.Port != 2222 || got.User != "deploy" {
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

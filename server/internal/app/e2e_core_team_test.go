package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The team half of TestE2ECore. Server .34 (2026-11-16) made team-owned machines unreachable while personal ones
// worked, and an ENV regression went unseen for days: both passed every unit and PostgreSQL test. So the owner
// (usr_e2e) transfers e2e-t to the seeded paid team (team-owned: `pb team machine share` or a team enrollment
// token would only bind a personal machine to the team), grants itself and the member (usr_e2e2, machine e2e-d) exact
// capabilities, and every phase asserts exec both ways, pb connect, a --team preview and tunnel (the edge gates them) of the team
// machine, the team ENV value on it (and never the owner's personal one), and the personal value on e2e-a.
// Prior art: tools/device-smoke/team_machines.py and xplat_envteam.py run the same commands on real devices.

const (
	e2eMember       = "usr_e2e2"
	e2ePersonalName = "E2E_PERSONAL"
	e2eTeamName     = "E2E_TEAM"
)

// setupTeam runs once, after pairing: the transfer, grants, ENV vault and one personal and one team value.
func (e *e2eCore) setupTeam() {
	if len(e.machines) < 4 {
		return
	}
	a, b, c := e.machines[0], e.machines[1], e.machines[2]
	// find returns c's id in the workspace's machine list and the team that owns it ("" when personal).
	find := func(workspace string) (id, ownerTeam string, err error) {
		out, err := e.pb(a, "--workspace", workspace, "machine", "list", "--json")
		if err != nil {
			return "", "", err
		}
		var listed struct {
			Machines []struct {
				ID, Alias   string
				OwnerTeamID string `json:"owner_team_id"`
			}
		}
		if err := json.Unmarshal([]byte(out), &listed); err != nil {
			return "", "", fmt.Errorf("machine list: %w: %s", err, lastBytes(out, 300))
		}
		for _, m := range listed.Machines {
			if m.Alias == c {
				return m.ID, m.OwnerTeamID, nil
			}
		}
		return "", "", fmt.Errorf("%s is not in the %s machine list: %s", c, workspace, lastBytes(out, 400))
	}
	var machineID string
	e.step(fmt.Sprintf("team: transfer %s to %s", c, e2eTeam), func() error {
		id, _, err := find("personal")
		if err != nil {
			return err
		}
		machineID = id
		return e.teamWrite(a, "team", "machine", "transfer-to-team", e2eTeam, machineID, "--confirm", machineID)
	})
	e.step(fmt.Sprintf("team: %s is owned by %s", c, e2eTeam), func() error {
		id, owner, err := find(e2eTeam)
		if err == nil && (id != machineID || owner != e2eTeam) {
			err = fmt.Errorf("%s lists as %s owned by %q, want %s owned by %s", c, id, owner, machineID, e2eTeam)
		}
		return err
	})
	e.step("team: grant the owner exact capabilities", func() error {
		return e.teamWrite(a, "team", "machine", "grant", e2eTeam, machineID, "--member", "usr_e2e",
			"--capability", "terminal,exec,managed_ssh,files,preview_manage,tunnel_manage")
	})
	e.step("team: grant the member terminal, exec and files", func() error {
		return e.teamWrite(a, "team", "machine", "grant", e2eTeam, machineID, "--member", e2eMember, "--capability", "terminal,exec,files")
	})
	// Server .34-era break (c3c381c35): the owner's CLI got a team-owned host's scopes at max(personal policy
	// version, team generation) on one side and the team generation on the other, and the relay forwards only
	// matching generations. Saving the personal policy past the team generation makes the relayed phase see it.
	e.step("team: the owner's personal policy version ahead of the team generation", func() error {
		for i := range 30 { // each save a real change (an identical one is no new version): one more empty tag
			source := fmt.Sprintf(`{"rules": [], "tags": {"tag:e2e-%d": []}}`, i)
			if out, err := e.pbStdin(a, source, ".local/bin/pb", "--workspace", "personal", "policy", "set", "-", "--message", "e2e-"+strconv.Itoa(i), "--yes"); err != nil {
				return fmt.Errorf("policy set: %w: %s", err, lastBytes(out, 300))
			}
		}
		versions, err := e.pb(a, "--workspace", "personal", "policy", "versions", "--json")
		if err != nil {
			return err
		}
		latest := int64(0)
		for _, m := range regexp.MustCompile(`"version":\s*(\d+)`).FindAllStringSubmatch(versions, -1) {
			v, _ := strconv.ParseInt(m[1], 10, 64)
			latest = max(latest, v)
		}
		team, err := e.teamGeneration(a)
		if err != nil {
			return err
		}
		if latest <= team {
			return fmt.Errorf("personal policy version %d is not ahead of team generation %d: %s", latest, team, lastBytes(versions, 300))
		}
		e.t.Logf("personal policy version %d, team generation %d", latest, team)
		return nil
	})
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		e.t.Fatal(err)
	}
	password := base64.RawURLEncoding.EncodeToString(secret)
	e.step("ENV: vault on the owner's machines", func() error {
		for _, m := range []string{a, b, c} {
			if _, err := e.pbStdin(m, password, "bash", "-c", "umask 077 && cat >/home/pbt/vault.pw"); err != nil {
				return err
			}
		}
		if out, err := e.pb(a, "env", "init", "--password-file", "/home/pbt/vault.pw", "--recovery-file", "/home/pbt/vault.recovery", "--json"); err != nil {
			return fmt.Errorf("init: %w: %s", err, lastBytes(out, 300))
		}
		for _, m := range []string{b, c} {
			if out, err := e.pb(m, "env", "unlock", "--password-file", "/home/pbt/vault.pw", "--json"); err != nil {
				return fmt.Errorf("unlock on %s: %w: %s", m, err, lastBytes(out, 300))
			}
		}
		return nil
	})
	e.step("ENV: a personal value", func() error {
		_, err := e.pbStdin(a, "personal-ok", ".local/bin/pb", "env", "set", e2ePersonalName, "--value-stdin", "--json")
		return err
	})
	e.step("ENV: the team scope and a team value", func() error {
		if out, err := e.pb(a, "env", "team", "create", e2eTeam, "--json"); err != nil {
			return fmt.Errorf("env team create: %w: %s", err, lastBytes(out, 300))
		}
		_, err := e.pbStdin(a, "team-ok", ".local/bin/pb", "--workspace", e2eTeam, "env", "set", e2eTeamName, "--team", e2eTeam, "--value-stdin", "--json")
		return err
	})
}

// assertTeam is every path through the team machine; the first exec of a phase waits like assertCore's.
func (e *e2eCore) assertTeam(phase string) {
	if len(e.machines) < 4 {
		return
	}
	a, b, c, d := e.machines[0], e.machines[1], e.machines[2], e.machines[3]
	team := []string{"--workspace", e2eTeam}
	e.assertTeamExec(phase)
	for _, from := range []string{a, d} {
		e.step(fmt.Sprintf("%s: pb connect %s -> %s", phase, from, c), func() error { return e.connect(from, c) })
	}
	e.step(fmt.Sprintf("%s: team ENV value on %s", phase, c), func() error {
		err := waitFor(150*time.Second, func() error {
			return e.pbWant("team-ok", a, append(team, "exec", c, "--", "printenv", e2eTeamName)...)
		})
		if err != nil { // what the host was published and applied (names and states, never values)
			shown, showErr := e.pb(a, "env", "host", "show", "--machine", c, "--json")
			err = fmt.Errorf("%w; pb env host show on %s: %v %s", err, c, showErr, lastBytes(shown, 1500))
		}
		return err
	})
	e.step(fmt.Sprintf("%s: no personal ENV value on the team machine %s", phase, c), func() error {
		return e.pbWant("[none]", a, append(team, "exec", c, "--", "sh", "-c", `echo "[${`+e2ePersonalName+`:-none}]"`)...)
	})
	e.step(fmt.Sprintf("%s: personal ENV value on %s", phase, a), func() error {
		return waitFor(150*time.Second, func() error { return e.pbWant("personal-ok", b, "exec", a, "--", "printenv", e2ePersonalName) })
	})
	e.step(fmt.Sprintf("%s: team preview of the team machine %s gated at the edge", phase, c), func() error {
		marker := "preview-ok " + phase
		if err := e.serveMarker(c, "8080", marker); err != nil {
			return err
		}
		started := time.Now()
		out, err := e.run(3*time.Minute, "docker", "exec", "-u", "pbt", "-e", "HOME=/home/pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", "-w", "/home/pbt", c,
			".local/bin/pb", "preview", "8080", "--workspace", e2eTeam, "--team", "--background", "--ttl", "30m", "--json")
		e.t.Logf("%s: team preview answered after %s", phase, time.Since(started).Round(time.Millisecond))
		if err != nil {
			return err
		}
		return e.fetchThroughEdge("preview", out, marker, true)
	})
	e.step(fmt.Sprintf("%s: team tunnel of the team machine %s gated at the edge", phase, c), func() error {
		// A named team tunnel, as tools/device-smoke/xplat_team.py makes one (a deleted name stays taken: one per phase).
		marker := "tunnel-ok " + phase
		if err := e.serveMarker(c, "8081", marker); err != nil {
			return err
		}
		name := "e2e-team-" + strings.ReplaceAll(phase, " ", "-")
		out, err := e.run(2*time.Minute, "docker", "exec", "-u", "pbt", "-e", "HOME=/home/pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", "-w", "/home/pbt", c,
			".local/bin/pb", "--workspace", e2eTeam, "tunnel", "create", name, "--port", "8081", "--team", "--json")
		if err != nil {
			return err
		}
		return e.fetchThroughEdge("tunnel create", out, marker, true)
	})
}

// assertTeamExec is exec into the team machine by its owner and by the granted member, and out of it.
func (e *e2eCore) assertTeamExec(phase string) {
	if len(e.machines) < 4 {
		return
	}
	a, c, d := e.machines[0], e.machines[2], e.machines[3]
	for _, from := range []string{a, d} {
		e.step(fmt.Sprintf("%s: team exec %s -> %s", phase, from, c), func() error {
			want := "team-exec-ok-" + from
			return waitFor(90*time.Second, func() error { return e.pbWant(want, from, "--workspace", e2eTeam, "exec", c, "--", "echo", want) })
		})
	}
	e.step(fmt.Sprintf("%s: exec from the team machine %s -> %s", phase, c, a), func() error {
		return waitFor(60*time.Second, func() error { return e.pbWant("from-team-ok", c, "exec", a, "--", "echo", "from-team-ok") })
	})
}

// teamWrite runs a pb team mutation with the team's current generation.
func (e *e2eCore) teamWrite(machine string, args ...string) error {
	generation, err := e.teamGeneration(machine)
	if err != nil {
		return err
	}
	out, err := e.pb(machine, append(args, "--generation", strconv.FormatInt(generation, 10), "--json")...)
	if err != nil {
		return fmt.Errorf("%w: %s", err, lastBytes(out, 300))
	}
	return nil
}

// teamGeneration is the team's current generation, from pb team get.
func (e *e2eCore) teamGeneration(machine string) (int64, error) {
	out, err := e.pb(machine, "team", "get", e2eTeam, "--json")
	if err != nil {
		return 0, err
	}
	var got struct {
		Generation int64
		Data       struct{ Generation int64 }
	}
	generation := int64(0)
	if json.Unmarshal([]byte(out), &got) == nil {
		generation = max(got.Generation, got.Data.Generation)
	}
	if generation == 0 {
		return 0, fmt.Errorf("team get printed no generation: %s", lastBytes(out, 300))
	}
	return generation, nil
}

// pbStdin runs a command in the machine as pbt with input on stdin (a value never on a command line).
func (e *e2eCore) pbStdin(machine, input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	base := []string{"exec", "-i", "-u", "pbt", "-e", "HOME=/home/pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", "-w", "/home/pbt", machine}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, lastBytes(string(out), 400))
	}
	return string(out), nil
}

// connect opens a terminal with pb connect under a pseudo-terminal (script), types a command and wants its
// result: the typed text shows $((6*7)), only the shell on the machine prints 42 (control_outage.py does the same).
func (e *e2eCore) connect(from, to string) error {
	marker := "connect-ok-" + from
	script := fmt.Sprintf(`(sleep 10; printf 'echo %[1]s-$((6*7))\n'; sleep 4; printf 'exit\n'; sleep 3) | timeout 60 script -qec '.local/bin/pb --workspace %[2]s connect %[3]s' /dev/null 2>&1 | tr -d '\r'`, marker, e2eTeam, to)
	return waitFor(3*time.Minute, func() error {
		out, err := e.run(80*time.Second, "docker", "exec", "-u", "pbt", "-e", "HOME=/home/pbt", "-e", "XDG_RUNTIME_DIR=/run/user/1000", "-e", "TERM=xterm", "-w", "/home/pbt", from, "bash", "-c", script)
		if !strings.Contains(out, marker+"-42") {
			return fmt.Errorf("pb connect %s printed no %s-42 (%v): %s", to, marker, err, lastBytes(out, 300))
		}
		return nil
	})
}

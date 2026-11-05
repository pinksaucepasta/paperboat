//go:build darwin || linux

package hostruntimecmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// hostJobs asks the service manager which process is each unit's or job's main
// one; that, not a process's executable, is what makes a process the install's.
func hostJobs(plan unixPurgePlan) func(context.Context) (map[int]string, error) {
	if plan.platform == "linux" {
		return func(ctx context.Context) (map[int]string, error) {
			output, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "--property=Id,MainPID", "paperboat*").Output()
			if err != nil {
				return nil, fmt.Errorf("list Paperboat systemd units: %w", err)
			}
			jobs := parseSystemdMainPIDs(string(output))
			// The user daemon is a unit of the account's own manager. Without that
			// manager (uninstall restored linger, nobody logged in) it runs nothing.
			manager := "user@" + strconv.Itoa(plan.uid) + ".service"
			if exec.CommandContext(ctx, "/usr/bin/systemctl", "is-active", "--quiet", manager).Run() != nil {
				return jobs, nil
			}
			account, err := user.LookupId(strconv.Itoa(plan.uid))
			if err != nil {
				return nil, err
			}
			output, err = exec.CommandContext(ctx, "/usr/bin/systemctl", "--user", "--machine="+account.Username+"@", "show", "--property=Id,MainPID", service.DaemonUnitPrefix+"*").Output()
			if err != nil {
				return nil, fmt.Errorf("read the user daemon unit of %s: %w", account.Username, err)
			}
			for pid, unit := range parseSystemdMainPIDs(string(output)) {
				jobs[pid] = unit
			}
			return jobs, nil
		}
	}
	return func(ctx context.Context) (map[int]string, error) {
		output, err := exec.CommandContext(ctx, "/bin/launchctl", "list").Output()
		if err != nil {
			return nil, fmt.Errorf("list launchd jobs: %w", err)
		}
		jobs := parseLaunchctlList(string(output))
		// The user agents live in the account's GUI domain, which exists only while it is logged in:
		// one per daemon generation, named by the plists in the account's LaunchAgents.
		account, err := user.LookupId(strconv.Itoa(plan.uid))
		if err != nil {
			return nil, err
		}
		plists, err := filepath.Glob(filepath.Join(account.HomeDir, "Library", "LaunchAgents", service.DaemonLabel+".*.plist"))
		if err != nil {
			return nil, err
		}
		for _, plist := range plists {
			label := strings.TrimSuffix(filepath.Base(plist), ".plist")
			output, err = exec.CommandContext(ctx, "/bin/launchctl", "print", "gui/"+strconv.Itoa(plan.uid)+"/"+label).Output()
			var exit *exec.ExitError
			if err != nil && !errors.As(err, &exit) {
				return nil, err
			}
			if pid := parseLaunchctlPrintPID(string(output)); err == nil && pid > 0 {
				jobs[pid] = label
			}
		}
		return jobs, nil
	}
}

// hostImageInodes lists the text-mapped inodes of a macOS process (its image
// first); Linux compares /proc/<pid>/exe in the process listing instead.
func hostImageInodes(plan unixPurgePlan) func(context.Context, int) ([]uint64, error) {
	if plan.platform == "linux" {
		return nil
	}
	return func(ctx context.Context, pid int) ([]uint64, error) {
		output, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-Fi").Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, nil // the process exited: nothing maps an image
		}
		if err != nil {
			return nil, err
		}
		return parseLsofInodes(string(output)), nil
	}
}

// parseSystemdMainPIDs reads `systemctl show --property=Id,MainPID` blocks.
func parseSystemdMainPIDs(output string) map[int]string {
	jobs := map[int]string{}
	unit, pid := "", 0
	flush := func() {
		if unit != "" && pid > 0 {
			jobs[pid] = unit
		}
		unit, pid = "", 0
	}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "Id="):
			unit = strings.TrimPrefix(line, "Id=")
		case strings.HasPrefix(line, "MainPID="):
			pid, _ = strconv.Atoi(strings.TrimPrefix(line, "MainPID="))
		}
	}
	flush()
	return jobs
}

// parseLaunchctlList reads `launchctl list`: PID, last status, label; "-" when not running.
func parseLaunchctlList(output string) map[int]string {
	jobs := map[int]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 0 {
			jobs[pid] = fields[2]
		}
	}
	return jobs
}

// parseLaunchctlPrintPID reads the "pid = N" line of `launchctl print`.
func parseLaunchctlPrintPID(output string) int {
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "pid = "); ok {
			pid, _ := strconv.Atoi(value)
			return pid
		}
	}
	return 0
}

// parseLsofInodes reads the "i<inode>" fields of `lsof -F i` output.
func parseLsofInodes(output string) []uint64 {
	var inodes []uint64
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, "i"); ok {
			if inode, err := strconv.ParseUint(value, 10, 64); err == nil {
				inodes = append(inodes, inode)
			}
		}
	}
	return inodes
}

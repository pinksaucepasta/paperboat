package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/helper/internal/selfupdate"
	"github.com/pinksaucepasta/paperboat/shared/tufroot"
)

// updateCommand is `pbh update`: it finds the newest signed release, shows current → latest, asks on a terminal,
// then runs the same steps as the console's Update button (updateStepsFor + runUpdate), including the rollback.
func updateCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("state-dir", defaultState, "installation state directory")
	yes := fs.Bool("yes", false, "update without asking")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(out, "update", fs)
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return commandError("unexpected update arguments")
	}
	if os.Geteuid() != 0 && *dir == defaultState {
		return elevated(ctx, append([]string{"update"}, args...), out)
	}
	node, err := containers.OpenUpdateNode(*dir)
	if err != nil {
		return withMessage("this node is not claimed yet: claim it in Dashboard → Network (pbh selfhost code prints the code), then run pbh update again", err)
	}
	check, cancel := context.WithTimeout(ctx, time.Minute)
	latest, err := selfupdate.Latest(check, node.ControlURL()+"/tuf", tufroot.TrustedRoot, filepath.Join(*dir, "update", "tuf"), runtime.GOARCH, node.DownloadClient())
	cancel()
	if err != nil {
		return withMessage("could not find the latest signed release; check this host's network and retry", err)
	}
	if latest == version || selfupdate.Newer(version, latest) {
		fmt.Fprintf(out, "pbh %s is up to date.\n", version)
		return nil
	}
	fmt.Fprintf(out, "Update pbh %s → %s\n", version, latest)
	if !*yes {
		if !isTerminal(os.Stdin) {
			return commandError("not a terminal: run pbh update --yes to update without asking")
		}
		fmt.Fprint(out, "Update now? The relay and edge restart briefly. [y/N] ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if answer = answer[:max(0, len(answer)-1)]; answer != "y" && answer != "Y" && answer != "yes" {
			fmt.Fprintln(out, "Nothing changed.")
			return nil
		}
	}
	if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", updateUnit).Run() == nil {
		return commandError("an update is already running (started from the dashboard); wait for it to finish")
	}
	signal.Ignore(syscall.SIGHUP) // a dropped SSH session must not stop a swap half way
	steps, err := updateStepsFor(ctx, node, *dir, latest, func(state, detail, reference string) {
		fmt.Fprintf(out, "update %s: %s %s %s\n", latest, state, detail, reference)
	})
	if err != nil {
		return err
	}
	if err := runUpdate(ctx, steps); err != nil {
		return withMessage("the update did not finish; the step lines above give the exact error and the previous version is restored when it had started", err)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

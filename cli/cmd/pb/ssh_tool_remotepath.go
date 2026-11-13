package main

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
)

// targetRsyncPath is the form of a drive-letter path (C:/Users/me/x) that rsync on a Windows target reads as absolute.
// rsync's change_dir treats only a leading "/" as absolute, so MSYS2's or Cygwin's rsync server joined "C:/..." onto
// its working directory ("/c/Users/me/C:/Users/me/x"). The target's own cygpath knows its flavour (/c/... for MSYS2,
// /cygdrive/c/... for Cygwin); a native rsync without cygpath keeps the path, as the local side does.
func targetRsyncPath(ctx context.Context, environment []string, destination managedssh.Destination, user, path string) (string, error) {
	if strings.ContainsAny(path, "\"%\r\n") { // cmd.exe would expand or split it; no Windows path holds these
		return path, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "ssh", user+"@"+destination.Host, `cygpath -u "`+path+`"`)
	command.Env = environment
	processlaunch.ConfigureBackground(command)
	out, err := command.Output()
	converted := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(converted, "/") {
		return path, nil
	}
	return converted, nil
}

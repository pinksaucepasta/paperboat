package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/helper/internal/selfupdate"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"github.com/pinksaucepasta/paperboat/shared/tufroot"
)

// updateUnit is the transient systemd unit an update runs in: it outlives the service restart the update causes.
const updateUnit = "paperboat-selfhost-update"

// updateSteps are the parts of an update that touch the network and the host; runUpdate orders them.
type updateSteps struct {
	report   func(state, detail, supportReference string)
	download func(ctx context.Context) (bundle string, err error)
	verify   func(ctx context.Context, bundle string) error
	extract  func(bundle string) (binary string, err error)
	backup   func() (restore func() error, err error)
	install  func(ctx context.Context, binary string) error // swaps in the new pbh and runs its health-checked install
}

// runUpdate downloads and verifies the release before changing anything; swaps keeping the old version; and
// restores it when the new one fails its health check. Every step and the exact failure reach the console/admin.
func runUpdate(ctx context.Context, s updateSteps) error {
	fail := func(state, step string, err error) error {
		reference := supportref.New()
		s.report(state, fmt.Sprintf("%s: %v", step, err), reference)
		return fmt.Errorf("%s (support reference %s): %w", step, reference, err)
	}
	s.report("downloading", "", "")
	bundle, err := s.download(ctx)
	if err != nil {
		return fail("failed", "download", err)
	}
	s.report("verifying", "", "")
	if err := s.verify(ctx, bundle); err != nil {
		return fail("failed", "signature verification refused the release; nothing was changed", err)
	}
	binary, err := s.extract(bundle)
	if err != nil {
		return fail("failed", "unpack the release; nothing was changed", err)
	}
	s.report("swapping", "", "")
	restore, err := s.backup()
	if err != nil {
		return fail("failed", "keep the current version; nothing was changed", err)
	}
	s.report("checking", "", "")
	if err := s.install(ctx, binary); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			return fail("failed", "the new version failed and restoring the previous one failed too", errors.Join(err, restoreErr))
		}
		return fail("rolled_back", "the new version failed its health check; the previous version runs again", err)
	}
	s.report("succeeded", "", "")
	return nil
}

// updateApply runs one requested update (pbh update-apply, inside updateUnit).
func updateApply(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("update-apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("state-dir", defaultState, "installation state directory")
	request := fs.String("request", "", "update request id")
	target := fs.String("to", "", "release version")
	if err := fs.Parse(args); err != nil || *request == "" || *target == "" || fs.NArg() != 0 {
		return commandError("usage: pbh update-apply --request ID --to VERSION")
	}
	node, err := containers.OpenUpdateNode(*dir)
	if err != nil {
		return err
	}
	work := filepath.Join(*dir, "update")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	arch := runtime.GOARCH
	steps := updateSteps{
		report: func(state, detail, reference string) {
			fmt.Fprintf(out, "update %s: %s %s %s\n", *target, state, detail, reference)
			report, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := node.Report(report, *request, state, detail, reference); err != nil {
				fmt.Fprintf(out, "update %s: reporting %s failed: %v\n", *target, state, err)
			}
		},
		download: func(ctx context.Context) (string, error) {
			path := filepath.Join(work, "bundle.tar.gz")
			url := node.ControlURL() + "/selfhost/versions/" + *target + "/paperboat-selfhost-linux-" + arch + ".tar.gz"
			return path, downloadBundle(ctx, node.DownloadClient(), url, path)
		},
		verify: func(ctx context.Context, bundle string) error {
			return selfupdate.Verify(ctx, node.ControlURL()+"/tuf", tufroot.TrustedRoot, filepath.Join(work, "tuf"), *target, arch, bundle, node.DownloadClient())
		},
		extract: func(bundle string) (string, error) { return extractPBH(bundle, filepath.Join(work, "release")) },
		backup:  func() (func() error, error) { return backupInstallation(ctx, *dir) },
		install: func(ctx context.Context, binary string) error { return installRelease(ctx, binary) },
	}
	return runUpdate(ctx, steps)
}

func downloadBundle(ctx context.Context, client *http.Client, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered HTTP %d for the release", response.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, io.LimitReader(response.Body, 128<<20))
	return errors.Join(err, file.Close())
}

// extractPBH unpacks only the package's own members (as install.sh does) and returns the pbh binary.
func extractPBH(bundle, dest string) (string, error) {
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	file, err := os.Open(bundle)
	if err != nil {
		return "", err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	archive := tar.NewReader(gz)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch header.Name {
		case "pbh", "LICENSE", "tailscale.LICENSE", "manifest.json":
		default:
			return "", fmt.Errorf("unexpected package member %q", header.Name)
		}
		if header.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("package member %q is not a regular file", header.Name)
		}
		out, err := os.OpenFile(filepath.Join(dest, header.Name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(out, io.LimitReader(archive, 256<<20))
		if err = errors.Join(err, out.Close()); err != nil {
			return "", err
		}
	}
	binary := filepath.Join(dest, "pbh")
	if _, err := os.Stat(binary); err != nil {
		return "", errors.New("the release package has no pbh")
	}
	return binary, nil
}

// installedFiles are what an update changes and a rollback puts back, together: the pbh executables, the
// service unit, the image choice, the installation config, and the edge's store (the old image must read it).
func installedFiles(dir string) []string {
	return []string{filepath.Join(defaultBinaries, "pbh"), "/usr/local/bin/pbh", "/etc/systemd/system/paperboat-selfhost.service",
		filepath.Join(dir, "images.json"), filepath.Join(dir, "selfhost.json"), filepath.Join(dir, "state", "state.json")}
}

// backupInstallation keeps the current version's files; the returned restore puts them back and restarts it.
func backupInstallation(ctx context.Context, dir string) (func() error, error) {
	type kept struct {
		data   []byte
		mode   os.FileMode
		exists bool
	}
	saved := map[string]kept{}
	for _, path := range installedFiles(dir) {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			saved[path] = kept{}
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		saved[path] = kept{data: data, mode: info.Mode().Perm(), exists: true}
	}
	return func() error {
		recovery, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = exec.CommandContext(recovery, "systemctl", "stop", "paperboat-selfhost.service").Run()
		var err error
		for path, file := range saved {
			if !file.exists {
				if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					err = errors.Join(err, removeErr)
				}
				continue
			}
			err = errors.Join(err, writeFileAtomic(path, file.data, file.mode))
		}
		err = errors.Join(err, exec.CommandContext(recovery, "systemctl", "daemon-reload").Run())
		return errors.Join(err, exec.CommandContext(recovery, "systemctl", "restart", "paperboat-selfhost.service").Run())
	}, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temporary := path + ".update-restore"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// installRelease swaps in the new pbh and runs its install over the existing installation, which upgrades the
// images and waits until every component is ready again (its own failure restores the previous images).
func installRelease(ctx context.Context, binary string) error {
	data, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(defaultBinaries, "pbh"), "/usr/local/bin/pbh"} {
		if err := writeFileAtomic(path, data, 0o755); err != nil {
			return err
		}
	}
	install, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(install, filepath.Join(defaultBinaries, "pbh"), "install").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pbh install: %w: %s", err, lastLine(string(output)))
	}
	if err := exec.CommandContext(install, "systemctl", "is-active", "--quiet", "paperboat-selfhost.service").Run(); err != nil {
		return errors.New("the service is not running after the update")
	}
	return nil
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return lines[len(lines)-1]
}

// updateLoop is pbh's check-in: every minute it reports its version and starts an update an operator asked for.
// No inbound connection: the node asks. An update that stopped (pbh restarted, the host rebooted) is reported failed.
func updateLoop(ctx context.Context, dir string, out io.Writer) {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	var lastRefusal time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer.Reset(time.Minute)
		node, err := containers.OpenUpdateNode(dir)
		if err != nil {
			continue // not claimed yet
		}
		check, cancel := context.WithTimeout(ctx, 15*time.Second)
		request, err := node.Check(check, version)
		cancel()
		if errors.Is(err, containers.ErrUpdateRequired) && time.Since(lastRefusal) > time.Hour {
			lastRefusal = time.Now()
			fmt.Fprintln(out, err)
		}
		if err != nil || request == nil {
			continue
		}
		if request.State == "requested" {
			start := exec.CommandContext(ctx, "systemd-run", "--unit="+updateUnit, "--collect", "--quiet",
				filepath.Join(defaultBinaries, "pbh"), "update-apply", "--state-dir", dir, "--request", request.ID, "--to", request.TargetVersion)
			if output, err := start.CombinedOutput(); err != nil {
				reference := supportref.New()
				_ = node.Report(ctx, request.ID, "failed", "start the update: "+lastLine(string(output)), reference)
			}
			continue
		}
		if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", updateUnit).Run() != nil {
			_ = node.Report(ctx, request.ID, "failed", "the update stopped before it finished (pbh restarted or the host rebooted); press Update again", supportref.New())
		}
	}
}

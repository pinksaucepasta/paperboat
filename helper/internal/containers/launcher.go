package containers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const ownerLabel = "io.paperboat.pbh-owner"
const StartupTimeout = 45 * time.Second
const commandTimeout = 15 * time.Second

type Launcher struct {
	StateDir      string
	Images        Images
	ReadOnlyFiles []string
}

func (l *Launcher) Validate(ctx context.Context, capability, componentDir string) error {
	image, err := l.Images.Image(capability)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("Docker is unavailable; run the self-host installer")
	}
	if filepath.Dir(componentDir) != l.StateDir || filepath.Base(componentDir) != capability {
		return errors.New("runtime directory does not belong to this installation")
	}
	client, err := newNodeClient(capability, componentDir)
	if err != nil {
		return err
	}
	defer client.http.CloseIdleConnections()
	if _, err := docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return errors.New("connect Docker daemon failed; start Docker and retry")
	}
	b, err := docker(ctx, "image", "inspect", "--format", `{"Os":{{json .Os}},"Architecture":{{json .Architecture}}}`, image)
	if err != nil {
		pull, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, err = dockerWithDeadline(pull, "pull", image)
		cancel()
		if err != nil {
			return errors.New("pull runtime image failed; check registry access and image configuration")
		}
		b, err = docker(ctx, "image", "inspect", "--format", `{"Os":{{json .Os}},"Architecture":{{json .Architecture}}}`, image)
	}
	if err != nil {
		return errors.New("inspect runtime image failed")
	}
	var info struct{ Os, Architecture string }
	if json.Unmarshal(b, &info) != nil || info.Os != "linux" || info.Architecture != runtime.GOARCH {
		return errors.New("runtime image does not match this Linux host architecture")
	}
	return nil
}

func (l *Launcher) Run(ctx context.Context, capability, componentDir string, output io.Writer) (result error) {
	image, err := l.Images.Image(capability)
	if err != nil {
		return err
	}
	client, err := newNodeClient(capability, componentDir)
	if err != nil {
		return err
	}
	defer client.http.CloseIdleConnections()
	before, err := client.readiness(ctx)
	if err != nil {
		return err
	}
	owner := installationOwner(l.StateDir)
	name := "paperboat-pbh-" + owner + "-" + capability
	imageID, err := resolvedImage(ctx, image)
	if err != nil {
		return err
	}
	info, exists, err := inspectContainer(ctx, name)
	if err != nil {
		return err
	}
	if exists && info.Owner != owner {
		return errors.New("runtime container belongs to another installation; nothing changed")
	}
	adopted := exists && info.Running && info.ImageID == imageID
	if !adopted {
		if err := removeOwned(ctx, name, owner); err != nil {
			return err
		}
		if err := l.startContainer(ctx, capability, image); err != nil {
			return err
		}
	}

	defer func() {
		if ctx.Err() != nil && preservesHandoff(l.StateDir, capability, imageID) {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if err := removeOwned(cleanup, name, owner); err != nil {
			result = errors.Join(result, err)
		}
	}()
	logsCtx, stopLogs := context.WithCancel(ctx)
	logs := exec.CommandContext(logsCtx, "docker", "logs", "--follow", name)
	logs.Stdout, logs.Stderr = output, output
	logs.WaitDelay = 2 * time.Second
	if err := logs.Start(); err != nil {
		stopLogs()
		return errors.New("follow runtime container logs failed")
	}
	logsDone := make(chan error, 1)
	go func() { logsDone <- logs.Wait() }()
	defer func() { stopLogs(); <-logsDone }()
	ready, cancel := context.WithTimeout(ctx, StartupTimeout)
	previousEpoch := before.ProcessEpoch
	if adopted {
		previousEpoch = ""
	}
	err = waitReady(ready, name, owner, client, previousEpoch)
	cancel()
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Paperboat %s container ready.\n", capability)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			running, err := containerRunning(ctx, name, owner)
			if err != nil {
				return err
			}
			if !running {
				return errors.New("runtime container exited; supervisor will retry")
			}
		}
	}
}

func waitReady(ctx context.Context, name, owner string, client *nodeClient, previousEpoch string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		running, err := containerRunning(ctx, name, owner)
		if err != nil {
			return err
		}
		if !running {
			return errors.New("runtime container exited before readiness")
		}
		state, err := client.readiness(ctx)
		if err == nil && state.Ready && state.ProcessEpoch != previousEpoch {
			return nil
		}
		var response ResponseError
		if errors.As(err, &response) && response.Status == 401 {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("runtime container did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func installationOwner(dir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return hex.EncodeToString(sum[:8])
}

func containerRunning(ctx context.Context, name, owner string) (bool, error) {
	info, exists, err := inspectContainer(ctx, name)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if info.Owner != owner {
		return false, errors.New("container name belongs to another installation; nothing removed")
	}
	return info.Running, nil
}

type containerInfo struct {
	Owner   string `json:"owner"`
	Running bool   `json:"running"`
	ImageID string `json:"image_id"`
}

func inspectContainer(ctx context.Context, name string) (containerInfo, bool, error) {
	var info containerInfo
	// List existence separately so a daemon failure cannot be treated as an
	// absent container and silently replace its ownership.
	list, err := docker(ctx, "container", "ls", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return info, false, errors.New("inspect Docker container inventory failed")
	}
	if strings.TrimSpace(string(list)) == "" {
		return info, false, nil
	}
	b, err := docker(ctx, "inspect", "--format", `{"owner":{{json (index .Config.Labels "`+ownerLabel+`")}},"running":{{json .State.Running}},"image_id":{{json .Image}}}`, name)
	if err != nil || json.Unmarshal(b, &info) != nil {
		return info, false, errors.New("inspect runtime container ownership failed")
	}
	return info, true, nil
}

func removeOwned(ctx context.Context, name, owner string) error {
	info, exists, err := inspectContainer(ctx, name)
	if err != nil || !exists {
		return err
	}
	if info.Owner != owner {
		return errors.New("container name belongs to another installation; nothing removed")
	}
	if info.Running {
		if _, err := docker(ctx, "stop", "--time", "5", name); err != nil {
			return errors.New("stop owned runtime container failed; state retained")
		}
	}
	if _, err := docker(ctx, "rm", name); err != nil {
		return errors.New("remove stopped runtime container failed; state retained")
	}
	return nil
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return dockerWithDeadline(bounded, args...)
}

func dockerWithDeadline(ctx context.Context, args ...string) ([]byte, error) {
	var out cappedOutput
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if out.overflow {
		return nil, errors.New("Docker response exceeded its size limit")
	}
	return out.Bytes(), nil
}

type cappedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (1 << 20) - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, err := b.Buffer.Write(p)
	return n, err
}

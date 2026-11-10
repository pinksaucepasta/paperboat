//go:build windows

package machineguard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

const windowsGuardTask = `\Paperboat\MachineGuard`

func installGuard(ctx context.Context, executable, browserDomain string, undo **Undo) (resultErr error) {
	if !elevation.IsCurrentProcessElevated() {
		return errors.New("installing the machine guard requires an elevated administrator")
	}
	lifecycle, err := lockGuardLifecycle(DefaultStateDir)
	if err != nil {
		return err
	}
	defer lifecycle.Close()
	if err := recordInstallBrowserDomain(browserDomain); err != nil {
		return err
	}
	oldState, err := loadLoopbackState(DefaultStateDir)
	if err != nil {
		return err
	}
	if _, migrateErr := migrateLoopbackState(ctx, DefaultStateDir); migrateErr != nil {
		return migrateErr
	}
	defer func() {
		if resultErr != nil && !isTrustPending(resultErr) {
			resultErr = errors.Join(resultErr, writeLoopbackState(DefaultStateDir, oldState))
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}

	if !elevation.IsCurrentProcessElevated() {
		return errors.New("installing the machine guard requires an elevated administrator")
	}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		return errors.New("ProgramFiles is unavailable")
	}
	directory := filepath.Join(programFiles, "Paperboat", "MachineGuard")
	if err := protectedDirectory(directory, 0755); err != nil {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.CreateTemp(directory, ".pb-*.exe")
	if err != nil {
		return err
	}
	temporary := target.Name()
	defer os.Remove(temporary)
	if _, err = io.Copy(target, source); err == nil {
		err = target.Sync()
	}
	err = errors.Join(err, target.Close())
	if err != nil {
		return err
	}
	installed := filepath.Join(directory, "pb.exe")
	existing, err := ownedScheduledTask(ctx, installed)
	if err != nil {
		return err
	}
	rollback, err := snapshotGuardFiles(installed)
	if err != nil {
		return err
	}
	// The Undo outlives this call, so the prior task is kept in memory: as a file removed on return, the
	// restore after a failed update had nothing to register the prior guard from ("exit status 1").
	var priorTask string
	if existing {
		priorXML, queryErr := exec.CommandContext(ctx, "schtasks.exe", "/Query", "/TN", windowsGuardTask, "/XML").Output()
		if queryErr != nil {
			rollback.cleanup()
			return queryErr
		}
		priorTask = decodeTaskXML(priorXML)
	}
	candidateStarted := false
	candidateTaskCreated := false
	defer finishGuardInstall(ctx, rollback, &resultErr, undo, func(c context.Context) error {
		if !candidateStarted {
			return nil
		}
		return exec.CommandContext(c, "schtasks.exe", "/End", "/TN", windowsGuardTask).Run()
	}, func(c context.Context) error {
		if err := writeLoopbackState(DefaultStateDir, oldState); err != nil {
			return err
		}
		if !existing {
			if candidateTaskCreated {
				return exec.CommandContext(c, "schtasks.exe", "/Delete", "/TN", windowsGuardTask, "/F").Run()
			}
			return nil
		}
		priorTaskPath, err := writeGuardTaskFile(DefaultStateDir, priorTask)
		if err != nil {
			return err
		}
		defer os.Remove(priorTaskPath)
		for _, args := range [][]string{{"/Create", "/TN", windowsGuardTask, "/XML", priorTaskPath, "/F"}, {"/Run", "/TN", windowsGuardTask}} {
			if output, err := exec.CommandContext(c, "schtasks.exe", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("restore the prior machine guard scheduled task: %w: %s", err, bytes.TrimSpace(output))
			}
		}
		return nil
	})
	if existing {
		output, stopErr := exec.CommandContext(ctx, "schtasks.exe", "/End", "/TN", windowsGuardTask).CombinedOutput()
		if stopErr != nil {
			return fmt.Errorf("stop existing machine guard task: %w: %s", stopErr, bytes.TrimSpace(output))
		}

	}
	if err := PrepareLocalCARenewal(ctx, Config{StateDir: DefaultStateDir}); err != nil {
		return fmt.Errorf("renew local browser trust: %w", err)
	}
	// Pending browser trust is returned only after the guard task is ready.
	trustErr := prepareInstalledLocalCA(ctx, Config{StateDir: DefaultStateDir})
	if trustErr != nil && !isTrustPending(trustErr) {
		return trustErr
	}
	err = replaceGuardFile(temporary, installed)
	rollback.record(installed)
	if err != nil {
		return err
	}
	if err = protectedDirectory(directory, 0755); err != nil {
		return err
	}
	if err = protectedDirectory(DefaultStateDir, 0700); err != nil {
		return err
	}
	taskPath, err := writeGuardTaskFile(DefaultStateDir, taskXML(installed))
	if err != nil {
		return err
	}
	defer os.Remove(taskPath)
	for _, args := range [][]string{{"/Create", "/TN", windowsGuardTask, "/XML", taskPath, "/F"}, {"/Run", "/TN", windowsGuardTask}} {
		if args[0] == "/Run" {
			candidateStarted = true
		}
		output, runErr := exec.CommandContext(ctx, "schtasks.exe", args...).CombinedOutput()
		if runErr != nil {
			return fmt.Errorf("machine guard scheduled task: %w: %s", runErr, bytes.TrimSpace(output))
		}
		if args[0] == "/Create" {
			candidateTaskCreated = true
		}
	}
	if err := startGuardWithRetry(ctx, func(probeCtx context.Context) error {
		client, connectErr := Connect(probeCtx, DefaultSocket)
		if connectErr != nil {
			return connectErr
		}
		readyErr := client.ReplaceNames(probeCtx, nil)
		if closeErr := client.Close(); readyErr == nil {
			return closeErr
		}
		return readyErr
	}, func(restartCtx context.Context) error {
		// Not running (it already exited) is fine for /End; the /Run must work.
		_ = exec.CommandContext(restartCtx, "schtasks.exe", "/End", "/TN", windowsGuardTask).Run()
		if output, err := exec.CommandContext(restartCtx, "schtasks.exe", "/Run", "/TN", windowsGuardTask).CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, bytes.TrimSpace(output))
		}
		return nil
	}); err != nil {
		return err
	}
	return trustErr
}

func ownedScheduledTask(ctx context.Context, installed string) (bool, error) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return false, errors.New("SystemRoot is unavailable")
	}
	if _, err := os.Stat(filepath.Join(systemRoot, "System32", "Tasks", "Paperboat", "MachineGuard")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect machine guard task file: %w", err)
	}
	output, err := exec.CommandContext(ctx, "schtasks.exe", "/Query", "/TN", windowsGuardTask, "/XML").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect existing machine guard task: %w: %s", err, bytes.TrimSpace(output))
	}
	var task struct {
		Principals struct {
			Principal struct {
				UserID string `xml:"UserId"`
			} `xml:"Principal"`
		} `xml:"Principals"`
		Actions struct {
			Exec struct {
				Command   string `xml:"Command"`
				Arguments string `xml:"Arguments"`
			} `xml:"Exec"`
		} `xml:"Actions"`
	}
	output = bytes.Replace([]byte(decodeTaskXML(output)), []byte(`encoding="UTF-16"`), []byte(`encoding="UTF-8"`), 1)
	if err = xml.Unmarshal(output, &task); err != nil {
		return false, fmt.Errorf("parse existing machine guard task: %w", err)
	}
	if task.Principals.Principal.UserID != "S-1-5-18" || !strings.EqualFold(filepath.Clean(task.Actions.Exec.Command), filepath.Clean(installed)) || strings.TrimSpace(task.Actions.Exec.Arguments) != "daemon machine-guard run" {
		return false, errors.New("existing machine guard task is not owned by Paperboat")
	}
	return true, nil
}

// decodeTaskXML reads what `schtasks /Query /XML` printed: UTF-16 with a byte order mark, or plain bytes.
func decodeTaskXML(output []byte) string {
	if len(output) < 2 || output[0] != 0xff || output[1] != 0xfe {
		return string(output)
	}
	words := make([]uint16, (len(output)-2)/2)
	for index := range words {
		words[index] = binary.LittleEndian.Uint16(output[2+index*2:])
	}
	return string(utf16.Decode(words))
}

// writeGuardTaskFile writes a task definition as `schtasks /Create /XML` reads it (UTF-16 with a byte order
// mark) into directory and returns the file; the caller removes it.
func writeGuardTaskFile(directory, definition string) (string, error) {
	file, err := os.CreateTemp(directory, ".task-*.xml")
	if err != nil {
		return "", err
	}
	words := utf16.Encode([]rune(definition))
	encoded := make([]byte, 2+len(words)*2)
	encoded[0], encoded[1] = 0xff, 0xfe
	for index, value := range words {
		encoded[2+index*2], encoded[3+index*2] = byte(value), byte(value>>8)
	}
	_, err = file.Write(encoded)
	if err = errors.Join(err, file.Close()); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}
func taskXML(executable string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(executable))
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
<Triggers><BootTrigger><Enabled>true</Enabled></BootTrigger></Triggers>
<Principals><Principal id="System"><UserId>S-1-5-18</UserId><RunLevel>HighestAvailable</RunLevel></Principal></Principals>
<Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>
<Actions Context="System"><Exec><Command>` + escaped.String() + `</Command><Arguments>daemon machine-guard run</Arguments></Exec></Actions></Task>`
}

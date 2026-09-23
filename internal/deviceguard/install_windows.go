//go:build windows

package deviceguard

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
	"time"
	"unicode/utf16"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

const windowsGuardTask = `\Paperboat\DeviceGuard`

func Install(ctx context.Context, executable string, configuredCIDR ...string) (resultErr error) {
	if !elevation.IsCurrentProcessElevated() {
		return errors.New("installing the device guard requires an elevated administrator")
	}
	lifecycle, err := lockGuardLifecycle(DefaultStateDir)
	if err != nil {
		return err
	}
	defer lifecycle.Close()
	oldState, err := loadLoopbackState(DefaultStateDir)
	if err != nil {
		return err
	}
	cidr := oldState.Active
	if len(configuredCIDR) > 0 && strings.TrimSpace(configuredCIDR[0]) != "" {
		cidr = configuredCIDR[0]
	}
	cidr, fence, err := prepareLoopbackCIDRChange(ctx, cidr)
	if err != nil {
		return err
	}
	if fence != nil {
		defer fence.Close()
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, writeLoopbackState(DefaultStateDir, oldState))
		}
	}()
	if err := storeLoopbackCIDR(DefaultStateDir, cidr); err != nil {
		return err
	}

	if !elevation.IsCurrentProcessElevated() {
		return errors.New("installing the device guard requires an elevated administrator")
	}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		return errors.New("ProgramFiles is unavailable")
	}
	directory := filepath.Join(programFiles, "Paperboat", "DeviceGuard")
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
	if existing {
		output, stopErr := exec.CommandContext(ctx, "schtasks.exe", "/End", "/TN", windowsGuardTask).CombinedOutput()
		if stopErr != nil {
			return fmt.Errorf("stop existing device guard task: %w: %s", stopErr, bytes.TrimSpace(output))
		}
	}
	replaceDeadline := time.Now().Add(5 * time.Second)
	for {
		err = replaceStateFile(temporary, installed)
		if err == nil {
			break
		}
		if !existing || time.Now().After(replaceDeadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err = protectedDirectory(directory, 0755); err != nil {
		return err
	}
	if err = protectedDirectory(DefaultStateDir, 0700); err != nil {
		return err
	}
	taskFile, err := os.CreateTemp(DefaultStateDir, ".task-*.xml")
	if err != nil {
		return err
	}
	taskPath := taskFile.Name()
	defer os.Remove(taskPath)
	xmlBody := utf16.Encode([]rune(taskXML(installed)))
	encoded := make([]byte, 2+len(xmlBody)*2)
	encoded[0], encoded[1] = 0xff, 0xfe
	for index, value := range xmlBody {
		encoded[2+index*2], encoded[3+index*2] = byte(value), byte(value>>8)
	}
	if _, err = taskFile.Write(encoded); err == nil {
		err = taskFile.Close()
	} else {
		_ = taskFile.Close()
	}
	if err != nil {
		return err
	}
	for _, args := range [][]string{{"/Create", "/TN", windowsGuardTask, "/XML", taskPath, "/F"}, {"/Run", "/TN", windowsGuardTask}} {
		output, runErr := exec.CommandContext(ctx, "schtasks.exe", args...).CombinedOutput()
		if runErr != nil {
			return fmt.Errorf("device guard scheduled task: %w: %s", runErr, bytes.TrimSpace(output))
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		client, connectErr := Connect(probeCtx, DefaultSocket)
		if connectErr == nil {
			readyErr := client.ReplaceNames(probeCtx, nil)
			closeErr := client.Close()
			cancel()
			if readyErr == nil {
				return closeErr
			}
			connectErr = readyErr
		} else {
			cancel()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device guard did not become ready: %w", connectErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func ownedScheduledTask(ctx context.Context, installed string) (bool, error) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return false, errors.New("SystemRoot is unavailable")
	}
	if _, err := os.Stat(filepath.Join(systemRoot, "System32", "Tasks", "Paperboat", "DeviceGuard")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect device guard task file: %w", err)
	}
	output, err := exec.CommandContext(ctx, "schtasks.exe", "/Query", "/TN", windowsGuardTask, "/XML").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("inspect existing device guard task: %w: %s", err, bytes.TrimSpace(output))
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
	if len(output) >= 2 && output[0] == 0xff && output[1] == 0xfe {
		words := make([]uint16, (len(output)-2)/2)
		for index := range words {
			words[index] = binary.LittleEndian.Uint16(output[2+index*2:])
		}
		output = []byte(string(utf16.Decode(words)))
	}
	output = bytes.Replace(output, []byte(`encoding="UTF-16"`), []byte(`encoding="UTF-8"`), 1)
	if err = xml.Unmarshal(output, &task); err != nil {
		return false, fmt.Errorf("parse existing device guard task: %w", err)
	}
	if task.Principals.Principal.UserID != "S-1-5-18" || !strings.EqualFold(filepath.Clean(task.Actions.Exec.Command), filepath.Clean(installed)) || strings.TrimSpace(task.Actions.Exec.Arguments) != "daemon device-guard run" {
		return false, errors.New("existing device guard task is not owned by Paperboat")
	}
	return true, nil
}
func taskXML(executable string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(executable))
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
<Triggers><BootTrigger><Enabled>true</Enabled></BootTrigger></Triggers>
<Principals><Principal id="System"><UserId>S-1-5-18</UserId><RunLevel>HighestAvailable</RunLevel></Principal></Principals>
<Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>
<Actions Context="System"><Exec><Command>` + escaped.String() + `</Command><Arguments>daemon device-guard run</Arguments></Exec></Actions></Task>`
}

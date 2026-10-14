//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

const windowsRuntimeReadyTimeout = 30 * time.Second

// runProduction attaches the legacy production-run entry point to the stable
// Windows hostd supervisor. Hostd owns process creation; this entry point must
// not start a second worker or bypass the hostd lifecycle fence.
func runProduction(ctx context.Context, output io.Writer) error {
	client, err := windowsHostdClient()
	if err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, windowsRuntimeReadyTimeout)
	defer cancel()
	announced := false
	for {
		status, statusErr := client.Active(readyCtx)
		if statusErr == nil && status.State == hostdproto.StateActive {
			if !announced {
				if _, writeErr := fmt.Fprintln(output, "pb machine runtime ready"); writeErr != nil {
					return runtimeFailure("machine runtime readiness could not be written", writeErr)
				}
				announced = true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		if announced {
			if statusErr != nil {
				return fmt.Errorf("Paperboat Windows host runtime became unavailable: %w", statusErr)
			}
			return errors.New("Paperboat Windows host runtime became inactive")
		}
		if readyCtx.Err() != nil {
			if statusErr != nil {
				return fmt.Errorf("wait for Paperboat Windows host runtime: %w", statusErr)
			}
			return errors.New("Paperboat Windows host runtime did not activate")
		}
		select {
		case <-readyCtx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func windowsHostdClient() (*hostdproto.Client, error) {
	endpoint := os.Getenv("PAPERBOAT_HOSTD_SOCKET")
	instance, configErr := currentWindowsRuntimeInstance()
	config := instance.config
	if endpoint == "" {
		if configErr != nil {
			return nil, configErr
		}
		endpoint = instance.layout.HostdSocket
	}
	tokenPath, tokenPathSet := os.LookupEnv("PAPERBOAT_HOSTD_TOKEN_FILE")
	if !tokenPathSet && configErr == nil {
		tokenPath = config.TokenFile
	}
	token, err := readWindowsHostdToken(tokenPath)
	if err != nil {
		return nil, err
	}
	client, err := hostdproto.NewClient(endpoint, token, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("open Paperboat Windows host-runtime control pipe: %w", err)
	}
	return client, nil
}

func windowsRuntimeInstallConfig() (hostinstall.WindowsRuntimeConfig, error) {
	instance, err := currentWindowsRuntimeInstance()
	if err != nil {
		return hostinstall.WindowsRuntimeConfig{}, fmt.Errorf("load protected Windows runtime installation: %w", err)
	}
	return instance.config, nil
}

func readWindowsHostdToken(path string) ([]byte, error) { return readWindowsHostdTokenForSID(path, "") }

func readWindowsHostdTokenForSID(path, enrolledSID string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("Paperboat Windows host-runtime token path is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != 32 {
		return nil, runtimeFailure("Paperboat Windows host-runtime token is unavailable", err)
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, runtimeFailure("Paperboat Windows host-runtime token is unsafe", err)
	}
	if valid, aclErr := checkWindowsHostdTokenACLForSID(path, enrolledSID); aclErr != nil || !valid {
		return nil, runtimeFailure("Paperboat Windows host-runtime token permissions are unsafe", aclErr)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, runtimeFailure("opening the runtime token file failed", err)
	}
	return readRuntimeToken(file)
}

func validWindowsHostdTokenACL(path string) bool { return validWindowsHostdTokenACLForSID(path, "") }

func validWindowsHostdTokenACLForSID(path, enrolledSID string) bool {
	valid, err := checkWindowsHostdTokenACLForSID(path, enrolledSID)
	return err == nil && valid
}
func checkWindowsHostdTokenACLForSID(path, enrolledSID string) (bool, error) {
	if enrolledSID == "" {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return false, err
		}
		if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
			return false, nil
		}
		enrolledSID = user.User.Sid.String()
	}
	sid, err := windows.StringToSid(enrolledSID)
	if err != nil {
		return false, err
	}
	if sid == nil || !sid.IsValid() {
		return false, nil
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return false, err
	}
	owned, err := windowssecurity.CheckOwnerMatchesSID(path, system)
	if err != nil || !owned {
		return false, err
	}
	want := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if enrolledSID != "S-1-5-18" {
		want += "(A;;FR;;;" + enrolledSID + ")"
	}
	return windowssecurity.CheckProtectedDACLMatches(path, want)
}

func windowsHostdTokenDACL(value string) string {
	index := strings.Index(value, "D:")
	if index < 0 {
		return ""
	}
	open := strings.IndexByte(value[index:], '(')
	if open < 0 {
		return ""
	}
	// Windows may materialize the inherited-ACL marker alongside the protected
	// DACL marker. Protection is checked separately above; compare only the
	// ACE sequence so no extra principal is admitted.
	return "D:" + value[index+open:]
}

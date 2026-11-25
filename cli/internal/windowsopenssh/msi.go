package windowsopenssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/httptransport"
)

// MSIPackage is Microsoft's signed installer of the approved OpenSSH: the file winget itself downloads, at the same hash.
type MSIPackage struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

const maxMSISize = 32 << 20

var openMSI = func(ctx context.Context, url string) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: httptransport.Default(), CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != "https" || len(via) >= 5 {
			return errors.New("refused redirect of the OpenSSH download")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, errors.Join(fmt.Errorf("OpenSSH download answered HTTP %d", response.StatusCode), response.Body.Close())
	}
	return response.Body, nil
}

// installWithMSI installs OpenSSH on a machine without a usable winget (Windows Server, Sandbox, LocalSystem). Like the
// winget package it stays installed when Paperboat is uninstalled: other programs may use it by then.
func installWithMSI(ctx context.Context, config Config, force bool) (resultErr error) {
	architecture := config.Architecture
	if architecture == "" {
		architecture = "amd64"
	}
	msi, ok := compatibility.MSI[architecture]
	if !ok {
		return operationFailure("no approved OpenSSH installer exists for this processor", ErrInstallerUnavailable, nil)
	}
	directory, err := msiDirectory()
	if err != nil {
		return operationFailure("the Program Files directory could not be located", ErrInstallFailed, err)
	}
	if err := removeMSIDirectory(); err != nil { // an interrupted attempt's
		return err
	}
	if err := createProtectedDirectory(directory); err != nil {
		return operationFailure("Windows could not create a protected directory for the OpenSSH installer", ErrInstallFailed, err)
	}
	defer func() {
		if err := removeMSIDirectory(); resultErr == nil {
			resultErr = err
		}
	}()
	path := filepath.Join(directory, "openssh.msi")
	if err := downloadMSI(ctx, msi, path); err != nil {
		return err
	}
	if err := verifyMSISignature(ctx, config, path); err != nil {
		return err
	}
	installCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	msiexec, err := msiexecPath()
	if err != nil {
		return operationFailure("Windows Installer could not be located", ErrInstallFailed, err)
	}
	// No ADDLOCAL: the default is Client and Server, and Paperboat needs ssh.exe and sftp.exe from Client.
	run := func(action string) error {
		_, err := config.Runner.Run(installCtx, msiexec, action, path, "/qn", "/norestart")
		if qualificationExitCode(err) == 3010 { // installed; a restart is pending
			return nil
		}
		return err
	}
	failed := func(message string, err error) error {
		if qualificationExitCode(err) == 1618 {
			message = "Windows Installer is busy with another installation; run the command again when it has finished"
		}
		return operationFailure(message, ErrInstallFailed, err)
	}
	// /fa restores missing or damaged files but stops the running sshd, so only an explicit repair may do it: an install
	// or update over SSH would cut off the machine (winpool-2, 2026-11-26).
	if force {
		if err := run("/fa"); err == nil {
			return nil
		} else if qualificationExitCode(err) != 1605 { // 1605: not installed
			return failed("Windows Installer could not repair the approved OpenSSH package", err)
		}
	}
	if err := run("/i"); err != nil {
		return failed("Windows Installer could not install the approved OpenSSH package", err)
	}
	return nil
}

func removeMSIDirectory() error {
	directory, err := msiDirectory()
	if err == nil {
		err = os.RemoveAll(directory)
	}
	if err != nil {
		return operationFailure("Windows could not remove the downloaded OpenSSH installer", ErrInstallFailed, err)
	}
	return nil
}

func downloadMSI(ctx context.Context, msi MSIPackage, path string) error {
	downloadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	failed := func(err error) error {
		return operationFailure("could not download Microsoft's OpenSSH installer; check the network and proxy, then run the command again", ErrInstallFailed, err)
	}
	body, err := openMSI(downloadCtx, msi.URL)
	if err != nil {
		return failed(err)
	}
	defer body.Close()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return failed(err)
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(body, maxMSISize+1))
	if err := errors.Join(err, file.Close()); err != nil {
		return failed(err)
	}
	if written > maxMSISize || hex.EncodeToString(digest.Sum(nil)) != msi.SHA256 {
		return operationFailure("the downloaded OpenSSH installer is not the approved file", ErrUntrustedBinary, nil)
	}
	return nil
}

func verifyMSISignature(ctx context.Context, config Config, path string) error {
	script := securityModuleImport + "$s=Get-AuthenticodeSignature -LiteralPath '" + strings.ReplaceAll(path, "'", "''") + "';" +
		"if($s.Status -ne 'Valid' -or $s.SignerCertificate.Subject -notlike '*, O=Microsoft Corporation, *'){exit 41}"
	if _, err := config.Runner.Run(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script); err != nil {
		if ctx.Err() != nil {
			return operationFailure("Windows OpenSSH installer verification was interrupted", nil, errors.Join(context.Cause(ctx), err))
		}
		return operationFailure("the downloaded OpenSSH installer is not signed by Microsoft", ErrUntrustedBinary, err)
	}
	return nil
}

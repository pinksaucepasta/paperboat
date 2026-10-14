//go:build darwin

package workerupdate

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
)

// ExtractDarwinPackage keeps native archive conversion on pipes. All disk writes
// are performed by the bounded, path-validating stream reader.
func ExtractDarwinPackage(ctx context.Context, packagePath, extractionRoot string) (result string, resultErr error) {
	if ctx == nil || !canonicalRegularPackagePath(packagePath) || !canonicalExtractionRoot(extractionRoot) {
		return "", errPackageInstall
	}
	if err := validateDarwinXARHeader(packagePath); err != nil {
		return "", err
	}
	if err := validateDarwinExtractionRoot(extractionRoot); err != nil {
		return "", err
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	expanded := filepath.Join(extractionRoot, "expanded")
	if err := os.Mkdir(expanded, 0700); err != nil {
		return "", errPackageInstall
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.RemoveAll(expanded))
		}
	}()
	if err := convertDarwinArchive(bounded, "@"+packagePath, nil, func(stream io.Reader) error {
		return extractDarwinOuterStream(stream, expanded)
	}); err != nil {
		return "", err
	}
	if err := validateDarwinPackageInfo(filepath.Join(expanded, "PackageInfo")); err != nil {
		return "", err
	}
	compressedPayload := filepath.Join(expanded, ".compressed-payload")
	if err := os.Rename(filepath.Join(expanded, "Payload"), compressedPayload); err != nil {
		return "", errPackageInstall
	}
	payload, err := os.Open(compressedPayload)
	if err != nil {
		return "", errPackageInstall
	}
	defer payload.Close()
	payloadRoot := filepath.Join(expanded, "Payload")
	if err := os.Mkdir(payloadRoot, 0700); err != nil {
		return "", errPackageInstall
	}
	if err := convertDarwinArchive(bounded, "@-", payload, func(stream io.Reader) error {
		return extractDarwinPayloadStream(stream, payloadRoot)
	}); err != nil {
		return "", err
	}
	if err := os.Remove(compressedPayload); err != nil {
		return "", err
	}
	return filepath.Join(payloadRoot, strings.TrimPrefix(darwinPackageHelperPath, "./")), nil
}

func convertDarwinArchive(ctx context.Context, archive string, input io.Reader, consume func(io.Reader) error) error {
	bounded, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(bounded, "/usr/bin/tar", "-c", "--format=pax", "-f", "-", archive)
	processlaunch.ConfigureBackground(command)
	command.Stdin = input
	command.Stderr = io.Discard
	command.WaitDelay = 250 * time.Millisecond
	output, err := command.StdoutPipe()
	if err != nil {
		return errPackageInstall
	}
	if err := command.Start(); err != nil {
		output.Close()
		return errPackageInstall
	}
	err = consume(output)
	if err != nil {
		cancel()
	}
	output.Close()
	waitErr := command.Wait()
	if err != nil || waitErr != nil {
		return errors.Join(errPackageInstall, ctx.Err())
	}
	return nil
}

func validateDarwinXARHeader(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errPackageInstall
	}
	defer file.Close()
	var header [28]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return errPackageInstall
	}
	// Flat pkgbuild/productsign packages are XAR v1. Bound the native parser's
	// declared compressed and decoded table of contents before invoking it.
	compressed := binary.BigEndian.Uint64(header[8:16])
	decoded := binary.BigEndian.Uint64(header[16:24])
	if string(header[:4]) != "xar!" || binary.BigEndian.Uint16(header[4:6]) != 28 || binary.BigEndian.Uint16(header[6:8]) != 1 || compressed == 0 || decoded == 0 || compressed > uint64(maxPackageBOMBytes) || decoded > uint64(maxPackageBOMBytes) {
		return errPackageInstall
	}
	return nil
}

func canonicalRegularPackagePath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.EqualFold(filepath.Ext(path), ".pkg") {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}

func canonicalExtractionRoot(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validateDarwinExtractionRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errPackageInstall
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		return errPackageInstall
	}
	return nil
}

func validateDarwinPackageInfo(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > 64<<10 {
		return errPackageInstall
	}
	file, err := os.Open(path)
	if err != nil {
		return errPackageInstall
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		return errPackageInstall
	}
	var packageInfo struct {
		XMLName         xml.Name `xml:"pkg-info"`
		Identifier      string   `xml:"identifier,attr"`
		InstallLocation string   `xml:"install-location,attr"`
	}
	if err := xml.Unmarshal(body, &packageInfo); err != nil || packageInfo.Identifier != darwinPackageIdentifier || packageInfo.InstallLocation != darwinPackageInstallLocation {
		return errPackageInstall
	}
	return nil
}

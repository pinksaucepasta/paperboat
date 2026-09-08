//go:build darwin

package workerupdate

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/processlaunch"
)

var errPackageInstall = errors.New("macOS package extraction failed")

const (
	darwinPackageIdentifier      = "dev.pprbt.paperboat"
	darwinPackageInstallLocation = "/"
	darwinPackageCLIPath         = "./usr/local/bin/pb"
	darwinPackageHelperPath      = "./Library/PrivilegedHelperTools/Paperboat/bin/pb"
	darwinPackageCLITarget       = "/Library/PrivilegedHelperTools/Paperboat/bin/pb"
)

var runPkgutil = func(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "/usr/sbin/pkgutil", arguments...)
	processlaunch.ConfigureBackground(command)
	return command.CombinedOutput()
}

// ExtractDarwinPackage extracts a verified package into extractionRoot and
// returns the canonical executable payload. It deliberately does not invoke
// installer(8): the caller owns the durable runtime transition and removes
// extractionRoot after copying the verified payload into its staged slot.
func ExtractDarwinPackage(ctx context.Context, packagePath, extractionRoot string) (string, error) {
	if ctx == nil || !canonicalRegularPackagePath(packagePath) || !canonicalExtractionRoot(extractionRoot) {
		return "", errPackageInstall
	}
	if err := validateDarwinExtractionRoot(extractionRoot); err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(extractionRoot, "expanded")); err == nil {
		return "", errPackageInstall
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errPackageInstall
	}
	if err := validateDarwinPayload(ctx, packagePath); err != nil {
		return "", err
	}
	expandedPath := filepath.Join(extractionRoot, "expanded")
	if _, err := runPkgutil(ctx, "--expand-full", packagePath, expandedPath); err != nil {
		return "", errPackageInstall
	}
	if err := validateDarwinExtraction(expandedPath); err != nil {
		return "", err
	}
	if err := validateDarwinPackageInfo(filepath.Join(expandedPath, "PackageInfo")); err != nil {
		return "", err
	}
	payloadRoot := filepath.Join(expandedPath, "Payload")
	if err := validateDarwinPayloadFiles(payloadRoot); err != nil {
		return "", err
	}
	return filepath.Join(payloadRoot, strings.TrimPrefix(darwinPackageHelperPath, "./")), nil
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

func validateDarwinPayload(ctx context.Context, packagePath string) error {
	output, err := runPkgutil(ctx, "--payload-files", packagePath)
	if err != nil {
		return errPackageInstall
	}
	seen := make(map[string]struct{}, 2)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line {
		case ".", "./usr", "./usr/local", "./usr/local/bin", "./Library", "./Library/PrivilegedHelperTools", "./Library/PrivilegedHelperTools/Paperboat", "./Library/PrivilegedHelperTools/Paperboat/bin":
			// pkgbuild includes the canonical parent directories in its BOM.
			// No other directory or payload path belongs to this package.
			continue
		case darwinPackageCLIPath, darwinPackageHelperPath:
			if _, duplicate := seen[line]; duplicate {
				return errPackageInstall
			}
			seen[line] = struct{}{}
		default:
			return errPackageInstall
		}
	}
	if len(seen) != 2 {
		return errPackageInstall
	}
	return nil
}

func validateDarwinExtraction(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errPackageInstall
	}
	directories := map[string]struct{}{
		".": {}, "Payload": {}, "Payload/usr": {}, "Payload/usr/local": {}, "Payload/usr/local/bin": {},
		"Payload/Library": {}, "Payload/Library/PrivilegedHelperTools": {},
		"Payload/Library/PrivilegedHelperTools/Paperboat":     {},
		"Payload/Library/PrivilegedHelperTools/Paperboat/bin": {},
	}
	regularFiles := map[string]struct{}{
		"Bom": {}, "PackageInfo": {},
		"Payload/Library/PrivilegedHelperTools/Paperboat/bin/pb": {},
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errPackageInstall
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errPackageInstall
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			if relative != "Payload/"+strings.TrimPrefix(darwinPackageCLIPath, "./") {
				return errPackageInstall
			}
			target, err := os.Readlink(path)
			if err != nil || target != darwinPackageCLITarget {
				return errPackageInstall
			}
			return nil
		}
		if entry.IsDir() {
			if _, allowed := directories[relative]; !allowed {
				return errPackageInstall
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return errPackageInstall
		}
		if _, allowed := regularFiles[relative]; !allowed {
			return errPackageInstall
		}
		return nil
	})
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

func validateDarwinPayloadFiles(root string) error {
	cli := filepath.Join(root, strings.TrimPrefix(darwinPackageCLIPath, "./"))
	info, err := os.Lstat(cli)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return errPackageInstall
	}
	target, err := os.Readlink(cli)
	if err != nil || target != darwinPackageCLITarget {
		return errPackageInstall
	}
	helper := filepath.Join(root, strings.TrimPrefix(darwinPackageHelperPath, "./"))
	info, err = os.Lstat(helper)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxRuntimeBytes || info.Mode().Perm()&0o111 == 0 {
		return errPackageInstall
	}
	return nil
}

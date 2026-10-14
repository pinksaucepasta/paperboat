package workerupdate

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var errPackageInstall = errors.New("macOS package extraction failed")

const (
	darwinPackageIdentifier            = "dev.pprbt.paperboat"
	darwinPackageInstallLocation       = "/"
	darwinPackageCLIPath               = "./usr/local/bin/pb"
	darwinPackageHelperPath            = "./Library/PrivilegedHelperTools/Paperboat/bin/pb"
	darwinPackageCLITarget             = "/Library/PrivilegedHelperTools/Paperboat/bin/pb"
	maxPackageBOMBytes           int64 = 16 << 20
	maxPackageInfoBytes          int64 = 64 << 10
)

func extractDarwinOuterStream(stream io.Reader, root string) error {
	limits := map[string]int64{"Bom": maxPackageBOMBytes, "PackageInfo": maxPackageInfoBytes, "Payload": maxRuntimeBytes}
	return extractDarwinStream(stream, root, maxRuntimeBytes+maxPackageBOMBytes+maxPackageInfoBytes, 3, func(h *tar.Header) error {
		limit, ok := limits[h.Name]
		if !ok || h.Typeflag != tar.TypeReg || h.Size < 1 || h.Size > limit {
			return errPackageInstall
		}
		return nil
	}, func(seen map[string]bool) bool { return seen["Bom"] && seen["PackageInfo"] && seen["Payload"] })
}

func extractDarwinPayloadStream(stream io.Reader, root string) error {
	directories := map[string]bool{".": true, "usr": true, "usr/local": true, "usr/local/bin": true, "usr/local/share": true, "usr/local/share/man": true, "usr/local/share/man/man1": true, "Library": true, "Library/PrivilegedHelperTools": true, "Library/PrivilegedHelperTools/Paperboat": true, "Library/PrivilegedHelperTools/Paperboat/bin": true}
	helper := strings.TrimPrefix(darwinPackageHelperPath, "./")
	cli := strings.TrimPrefix(darwinPackageCLIPath, "./")
	manuals := 0
	return extractDarwinStream(stream, root, maxRuntimeBytes+int64(maxPackageManualCount)*maxPackageManualBytes, maxPackageManualCount+len(directories)+2, func(h *tar.Header) error {
		switch {
		case directories[h.Name]:
			if h.Typeflag != tar.TypeDir || h.Size != 0 {
				return errPackageInstall
			}
		case h.Name == cli:
			if h.Typeflag != tar.TypeSymlink || h.Linkname != darwinPackageCLITarget || h.Size != 0 {
				return errPackageInstall
			}
		case h.Name == helper:
			if h.Typeflag != tar.TypeReg || h.Size < 1 || h.Size > maxRuntimeBytes || h.Mode&0111 == 0 {
				return errPackageInstall
			}
		default:
			manuals++
			if !validPackageManualPath(h.Name) || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxPackageManualBytes || manuals > maxPackageManualCount {
				return errPackageInstall
			}
		}
		return nil
	}, func(seen map[string]bool) bool { return seen[helper] && seen[cli] })
}

// The reader, rather than the native converter, owns every write. Header sizes,
// paths, types, uniqueness and total decoded bytes are checked before creation.
func extractDarwinStream(stream io.Reader, root string, maxBytes int64, maxEntries int, validate func(*tar.Header) error, complete func(map[string]bool) bool) error {
	// Include bounded tar headers/padding and PAX metadata overhead in the pipe cap.
	limited := &io.LimitedReader{R: stream, N: maxBytes + int64(maxEntries)*(8192+1024) + 1}
	reader := tar.NewReader(limited)
	seen := make(map[string]bool)
	var total int64
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errPackageInstall
		}
		name := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if name == "" {
			name = "."
		}
		if filepath.IsAbs(name) || filepath.ToSlash(filepath.Clean(name)) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") || seen[name] || len(seen) >= maxEntries {
			return errPackageInstall
		}
		h.Name = name
		if err := validate(h); err != nil {
			return err
		}
		if h.Size > maxBytes-total {
			return errPackageInstall
		}
		total += h.Size
		seen[name] = true
		target := filepath.Join(root, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return errPackageInstall
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return errPackageInstall
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return errPackageInstall
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return errPackageInstall
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return errPackageInstall
			}
			_, copyErr := io.CopyN(file, reader, h.Size)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return errPackageInstall
			}
			if name == strings.TrimPrefix(darwinPackageHelperPath, "./") {
				if err := os.Chmod(target, 0700); err != nil {
					return errPackageInstall
				}
			}
		default:
			return errPackageInstall
		}
	}
	if !complete(seen) {
		return errPackageInstall
	}
	if _, err := io.Copy(io.Discard, limited); err != nil || limited.N == 0 {
		return errPackageInstall
	}
	return nil
}

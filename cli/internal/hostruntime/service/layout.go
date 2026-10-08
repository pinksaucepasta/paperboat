package service

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Layout is the fixed, root-owned layout used by Paperboat host installations.
// There is exactly one executable. Services invoke Binary through the explicit
// daemon entry point, and the updater atomically rotates Binary through the two
// release slots below. Callers cannot provide an install root: accepting one
// would turn the privileged updater into a generic file writer.
type Layout struct {
	Platform string
	// Instance is the immutable OS-user identity suffix for an enrolled host.
	// Empty is retained only for tests and migration inspection.
	Instance string

	InstallRoot    string
	ReleasesRoot   string
	Binary         string
	BinaryRollback string
	BinaryStaged   string

	UpdateStateRoot string
	HostdSocket     string
	UpdaterSocket   string
}

// WindowsUserInstance returns the fixed service/path suffix for an already
// validated Windows owner SID. Callers must validate the SID before using it.
func WindowsUserInstance(ownerSID string) (string, error) {
	if !strings.HasPrefix(ownerSID, "S-") || strings.ContainsAny(ownerSID, "\x00\r\n/\\") {
		return "", ErrInvalidDefinition
	}
	digest := sha256.Sum256([]byte(ownerSID))
	return "u" + hex.EncodeToString(digest[:12]), nil
}

func WindowsUserLayout(ownerSID string) (Layout, error) {
	instance, err := WindowsUserInstance(ownerSID)
	if err != nil {
		return Layout{}, err
	}
	layout, err := DefaultLayout("windows")
	if err != nil {
		return Layout{}, err
	}
	layout.Instance = instance
	layout.InstallRoot = windowsPathJoin(layout.InstallRoot, "users", instance)
	layout.ReleasesRoot = windowsPathJoin(layout.InstallRoot, "releases")
	layout.Binary = windowsPathJoin(layout.InstallRoot, "bin", "pb.exe")
	layout.BinaryRollback = windowsPathJoin(layout.ReleasesRoot, "pb.rollback.exe")
	layout.BinaryStaged = windowsPathJoin(layout.ReleasesRoot, "pb.staged.exe")
	layout.UpdateStateRoot = windowsPathJoin(layout.UpdateStateRoot, "users", instance)
	layout.HostdSocket += "-" + instance
	layout.UpdaterSocket = `\\.\pipe\PaperboatUpdatedControl-` + instance
	return layout, layout.Validate()
}

// UserLayout returns an independently owned privileged runtime layout for one
// Unix OS user. The numeric uid is supplied by the privileged enrollment
// boundary and cannot be redirected through account names or environment.
func UserLayout(platform string, uid int) (Layout, error) {
	if (platform != "linux" && platform != "darwin") || uid < 0 {
		return Layout{}, ErrInvalidDefinition
	}
	layout, err := DefaultLayout(platform)
	if err != nil {
		return Layout{}, err
	}
	instance := "u" + strconv.Itoa(uid)
	layout.Instance = instance
	layout.InstallRoot = filepath.Join(layout.InstallRoot, "users", instance)
	layout.ReleasesRoot = filepath.Join(layout.InstallRoot, "releases")
	layout.Binary = filepath.Join(layout.InstallRoot, "bin", "pb")
	layout.BinaryRollback = filepath.Join(layout.ReleasesRoot, "pb.rollback")
	layout.BinaryStaged = filepath.Join(layout.ReleasesRoot, "pb.staged")
	layout.UpdateStateRoot += "-" + instance
	layout.HostdSocket = filepath.Join(filepath.Dir(layout.HostdSocket)+"-"+instance, "hostd.sock")
	runtimeRoot := "/run"
	if platform == "darwin" {
		runtimeRoot = "/var/run"
	}
	layout.UpdaterSocket = filepath.Join(runtimeRoot, "paperboat-updated-"+instance, "control.sock")
	return layout, layout.Validate()
}

// DefaultLayout returns the fixed, supported native host layout. Windows uses
// a named pipe rather than pretending a filesystem socket is secure there.
func DefaultLayout(platform string) (Layout, error) {
	var installRoot, updateStateRoot, socketRoot string
	switch platform {
	case "linux":
		installRoot = "/usr/local/libexec/paperboat"
		updateStateRoot = "/var/lib/paperboat-updated"
		socketRoot = "/run/paperboat-hostd"
	case "darwin":
		installRoot = "/Library/PrivilegedHelperTools/Paperboat"
		updateStateRoot = "/Library/Application Support/Paperboat/updated"
		socketRoot = "/var/run/paperboat-hostd"
	case "windows":
		installRoot = `C:\Program Files\Paperboat`
		updateStateRoot = `C:\ProgramData\Paperboat\updated`
		socketRoot = `\\.\pipe\Paperboat`
	default:
		return Layout{}, ErrUnsupportedPlatform
	}
	join := path.Join
	if platform == "windows" {
		join = windowsPathJoin
	}
	releasesRoot := join(installRoot, "releases")
	layout := Layout{
		Platform: platform,

		InstallRoot:    installRoot,
		ReleasesRoot:   releasesRoot,
		Binary:         join(installRoot, "bin", "pb"),
		BinaryRollback: join(releasesRoot, "pb.rollback"),
		BinaryStaged:   join(releasesRoot, "pb.staged"),

		UpdateStateRoot: updateStateRoot,
		HostdSocket:     join(socketRoot, "hostd.sock"),
		UpdaterSocket:   join(updateStateRoot, "control.sock"),
	}
	if platform == "windows" {
		layout.Binary += ".exe"
		layout.BinaryRollback += ".exe"
		layout.BinaryStaged += ".exe"
		layout.HostdSocket = `\\.\pipe\PaperboatHostd`
		layout.UpdaterSocket = `\\.\pipe\PaperboatUpdatedControl`
	}
	if err := layout.Validate(); err != nil {
		return Layout{}, err
	}
	return layout, nil
}

func (l Layout) Validate() error {
	if l.Platform != "linux" && l.Platform != "darwin" && l.Platform != "windows" {
		return ErrUnsupportedPlatform
	}
	for _, path := range []string{
		l.InstallRoot, l.ReleasesRoot, l.Binary, l.BinaryRollback, l.BinaryStaged,
		l.UpdateStateRoot, l.HostdSocket, l.UpdaterSocket,
	} {
		if !absoluteForPlatform(l.Platform, path) {
			return ErrInvalidDefinition
		}
	}
	if l.Platform != "windows" && (filepath.Dir(l.HostdSocket) == l.UpdateStateRoot || filepath.Dir(l.HostdSocket) == l.ReleasesRoot) {
		return ErrInvalidDefinition
	}
	for _, path := range []string{l.BinaryRollback, l.BinaryStaged} {
		if !withinForPlatform(l.Platform, l.ReleasesRoot, path) {
			return ErrInvalidDefinition
		}
	}
	if !withinForPlatform(l.Platform, l.InstallRoot, l.ReleasesRoot) || !withinForPlatform(l.Platform, l.InstallRoot, l.Binary) {
		return ErrInvalidDefinition
	}
	paths := []string{l.Binary, l.BinaryRollback, l.BinaryStaged}
	seen := make(map[string]struct{}, len(paths))
	for _, value := range paths {
		key := value
		if l.Platform == "windows" {
			key = strings.ToLower(value)
		}
		if _, ok := seen[key]; ok {
			return ErrInvalidDefinition
		}
		seen[key] = struct{}{}
	}
	return nil
}

func absoluteForPlatform(platform, value string) bool {
	if platform != "windows" {
		return pathpkgIsCleanAbsolute(value)
	}
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && value[2] == '\\' || strings.HasPrefix(value, `\\.\pipe\`)
}

func windowsPathJoin(elements ...string) string {
	result := ""
	for _, element := range elements {
		if result == "" {
			result = strings.TrimRight(element, `\`)
			continue
		}
		result += `\` + strings.Trim(element, `\`)
	}
	return result
}

func withinForPlatform(platform, root, value string) bool {
	if platform != "windows" {
		if !pathpkgIsCleanAbsolute(root) || !pathpkgIsCleanAbsolute(value) {
			return false
		}
		root = strings.TrimRight(root, "/") + "/"
		return strings.HasPrefix(value, root)
	}
	root = strings.TrimRight(strings.ToLower(root), `\`) + `\`
	return strings.HasPrefix(strings.ToLower(value), root)
}

func pathpkgIsCleanAbsolute(value string) bool {
	return path.IsAbs(value) && path.Clean(value) == value
}

//go:build darwin || linux

package hostinstall

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/user"
	"runtime"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

const SchemaV1 = "paperboat.host-install/v1"

const journalSchemaV1 = "paperboat.host-install-journal/v1"

var (
	ErrInvalidRequest = errors.New("invalid privileged installation request")
	ErrNotPrivileged  = errors.New("privileged installation requires administrator approval")
)

// Request is the complete allowlist accepted by the privileged installer.
// It deliberately has no generic command, argument, path, or environment fields.
type Request struct {
	UpdateBundle           string                   `json:"update_bundle,omitempty"`
	Force                  bool                     `json:"force,omitempty"`
	SuppliedUpdate         bool                     `json:"supplied_update,omitempty"`
	Schema                 string                   `json:"schema"`
	Platform               string                   `json:"platform"`
	User                   string                   `json:"user"`
	UID                    int                      `json:"uid"`
	Group                  string                   `json:"group"`
	GID                    int                      `json:"gid"`
	Executable             string                   `json:"executable"`
	Artifact               bootstrap.ArtifactTarget `json:"artifact"`
	Source                 installsource.Source     `json:"source"`
	SessionOwnerGeneration string                   `json:"session_owner_generation,omitempty"`
	Home                   string                   `json:"home"`
	Path                   string                   `json:"path"`
	StateRoot              string                   `json:"state_root"`
	WorkspaceRoot          string                   `json:"workspace_root"`
	ControlURL             string                   `json:"control_url"`
	UserMachineID          string                   `json:"machine_id"`
	Shell                  string                   `json:"shell"`
	HelperListenAddress    string                   `json:"helper_listen_address"`
	EnrollmentPending      bool                     `json:"enrollment_pending"`
	InstallationGeneration int64                    `json:"installation_generation"`
}

func Decode(reader io.Reader) (Request, error) {
	var request Request
	if err := decodeInstallJSON(reader, &request, 128<<10); err != nil {
		return Request{}, err
	}
	return request, nil
}

func Validate(request Request, sudoUID int) error {
	if request.Schema != SchemaV1 || request.Platform != runtime.GOOS || !validRunIdentity(request) || sudoUID != request.UID ||
		request.EnrollmentPending || request.UserMachineID == "" || strings.ContainsAny(request.UserMachineID, "\x00\r\n") ||
		request.InstallationGeneration < 1 {
		return hostInstallFailure("identity contract", ErrInvalidRequest)
	}
	account, err := user.Lookup(request.User)
	if err != nil {
		return hostInstallFailure("user account", ErrInvalidRequest, err)
	}
	if account.Uid != strconv.Itoa(request.UID) || account.Gid != strconv.Itoa(request.GID) || account.HomeDir != request.Home {
		return hostInstallFailure("user account", ErrInvalidRequest)
	}
	group, err := user.LookupGroup(request.Group)
	if err != nil {
		return hostInstallFailure("user group", ErrInvalidRequest, err)
	}
	if group.Gid != strconv.Itoa(request.GID) {
		return hostInstallFailure("user group", ErrInvalidRequest)
	}
	if err := bootstrap.VerifyArtifactTarget(request.Artifact); request.Source.Distribution == installsource.Official && (err != nil || request.Artifact.Platform != request.Platform) {
		return hostInstallFailure("TUF target descriptor", ErrInvalidRequest, err)
	}
	if err := verifyArtifact(request.Executable, request.UID, request.Platform); err != nil {
		return hostInstallFailure("supplied executable file", ErrInvalidRequest, err)
	}
	if err := request.Source.Verify(request.Executable); err != nil || request.Source.Platform != request.Platform || request.Source.Architecture != runtime.GOARCH {
		return hostInstallFailure("target architecture", ErrInvalidRequest, err)
	}
	for _, path := range []string{request.Home, request.StateRoot, request.WorkspaceRoot} {
		if err := checkCanonicalOwnedDirectory(path, request.UID); err != nil {
			return hostInstallFailure("invalid installation directory", ErrInvalidRequest, err)
		}
	}
	if err := checkCanonicalExecutable(request.Shell); err != nil {
		return hostInstallFailure("invalid login shell", ErrInvalidRequest, err)
	}
	if !pathListValid(request.Path) {
		return hostInstallFailure("service PATH", ErrInvalidRequest)
	}
	parsed, err := url.Parse(request.ControlURL)
	if err != nil {
		return hostInstallFailure("control URL", ErrInvalidRequest, err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return hostInstallFailure("control URL", ErrInvalidRequest)
	}
	host, port, err := net.SplitHostPort(request.HelperListenAddress)
	if err != nil {
		return hostInstallFailure("helper listen address", ErrInvalidRequest, err)
	}
	if port == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return hostInstallFailure("helper listen address", ErrInvalidRequest)
	}
	return nil
}

func validRunIdentity(request Request) bool {
	return request.UID > 0 && request.GID > 0 || request.UID == 0 && request.GID == 0 && request.User == "root"
}

func (r Request) String() string {
	return fmt.Sprintf("%s for uid %d", r.Schema, r.UID)
}

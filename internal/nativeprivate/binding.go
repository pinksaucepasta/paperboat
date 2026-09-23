// Package nativeprivate owns the v1 authorization binding carried by native
// private preview and tunnel streams. It intentionally contains no edge or
// connector identity: those belong to the browser/public trust boundary.
package nativeprivate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	SchemaV1             = "paperboat.native-private-target/v1"
	HTTP3ConnectProtocol = "paperboat-private-http"
)

var ErrInvalid = errors.New("invalid native private target binding")

type Binding struct {
	UserID             string `json:"user_id,omitempty"`
	CLIClientSessionID string `json:"cli_client_session_id,omitempty"`
	AccessSessionID    string `json:"access_session_id,omitempty"`

	InstallationGeneration int64  `json:"installation_generation,omitempty"`
	BootID                 string `json:"boot_id,omitempty"`
	PolicyGeneration       int64  `json:"policy_generation,omitempty"`
	AnnouncementGeneration int64  `json:"announcement_generation,omitempty"`

	Schema             string    `json:"schema"`
	ResourceKind       string    `json:"resource_kind"`
	ResourceID         string    `json:"resource_id"`
	ResourceGeneration uint64    `json:"resource_generation"`
	RouteID            string    `json:"route_id"`
	RouteGeneration    uint64    `json:"route_generation"`
	TargetGeneration   uint64    `json:"target_generation"`
	OwnerEndpointID    string    `json:"owner_endpoint_id"`
	Protocol           string    `json:"protocol"`
	TargetScheme       string    `json:"target_scheme"`
	TargetAddress      string    `json:"target_address"`
	ExpiresAt          time.Time `json:"expires_at"`
}

func (b Binding) Validate(now time.Time) error {
	if b.Schema != SchemaV1 || !id(b.ResourceID) || !id(b.RouteID) || !id(b.OwnerEndpointID) || b.ResourceGeneration == 0 || b.RouteGeneration == 0 || b.TargetGeneration == 0 || !b.ExpiresAt.After(now) || b.ExpiresAt.Sub(now) > 5*time.Minute {
		return ErrInvalid
	}
	if b.ResourceKind != "preview" && b.ResourceKind != "tunnel" && b.ResourceKind != "device_service" || b.Protocol != "http" && b.Protocol != "tcp" {
		return ErrInvalid
	}
	if b.ResourceKind == "preview" && b.Protocol != "http" {
		return ErrInvalid
	}
	wantScheme := map[string]map[string]bool{"http": {"http": true, "https": true, "h2c": true}, "tcp": {"tcp": true}}
	if !wantScheme[b.Protocol][b.TargetScheme] || !literalLoopback(b.TargetAddress) {
		return ErrInvalid
	}

	if b.ResourceKind == "device_service" {
		_, port, _ := net.SplitHostPort(b.TargetAddress)
		if b.ResourceID != b.OwnerEndpointID || b.RouteID != "tcp:"+port || b.Protocol != "tcp" || !id(b.UserID) || !id(b.CLIClientSessionID) || !id(b.AccessSessionID) || b.InstallationGeneration < 1 || !id(b.BootID) || b.PolicyGeneration < 1 || b.AnnouncementGeneration < 1 {
			return ErrInvalid
		}
	} else if b.UserID != "" || b.CLIClientSessionID != "" || b.AccessSessionID != "" || b.InstallationGeneration != 0 || b.BootID != "" || b.PolicyGeneration != 0 || b.AnnouncementGeneration != 0 {
		return ErrInvalid
	}
	return nil
}

func Decode(raw []byte, now time.Time) (Binding, error) {
	if len(raw) == 0 || len(raw) > 4<<10 {
		return Binding{}, ErrInvalid
	}
	var binding Binding
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || decoder.Decode(&struct{}{}) != io.EOF || binding.Validate(now) != nil {
		return Binding{}, ErrInvalid
	}
	return binding, nil
}

func id(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}

func literalLoopback(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" && host != "::1" {
		return false
	}
	parsed, err := strconv.ParseUint(port, 10, 16)
	return err == nil && parsed > 0 && strconv.FormatUint(parsed, 10) == port
}

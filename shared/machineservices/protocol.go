// Package machineservices defines machine-service discovery and policy wire contracts.
package machineservices

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

const MaxServices = 64

var ErrInvalid = errors.New("invalid machine service request")

type Port struct {
	Port          int    `json:"port"`
	AddressFamily string `json:"address_family"`
}
type PublishedPort struct {
	Port          int    `json:"port"`
	AddressFamily string `json:"address_family"`
	BrowserURL    string `json:"browser_url,omitempty"`
}
type Snapshot struct {
	Schema                 string    `json:"schema"`
	InstallationGeneration int64     `json:"installation_generation"`
	BootID                 string    `json:"boot_id"`
	StartedAt              time.Time `json:"started_at"`
	Generation             int64     `json:"generation"`
	Services               []Port    `json:"services"`
}
type Policy struct {
	Schema              string `json:"schema"`
	MachineID           string `json:"machine_id"`
	Generation          int64  `json:"generation"`
	PersonalAutoEnabled bool   `json:"personal_auto_enabled"`
	TeamAutoEnabled     bool   `json:"team_auto_enabled"`
}
type PolicyUpdate struct {
	ExpectedGeneration  int64 `json:"expected_generation"`
	PersonalAutoEnabled bool  `json:"personal_auto_enabled"`
	TeamAutoEnabled     bool  `json:"team_auto_enabled"`
}
type RuntimePolicy struct {
	Generation                int64  `json:"generation"`
	AutomaticDiscoveryEnabled bool   `json:"automatic_discovery_enabled"`
	ExplicitPorts             []Port `json:"explicit_ports"`
}

func (p Port) Address() string {
	h := "127.0.0.1"
	if p.AddressFamily == "ipv6" {
		h = "::1"
	}
	return net.JoinHostPort(h, strconv.Itoa(p.Port))
}
func ValidPort(p Port) bool {
	return p.Port > 0 && p.Port <= 65535 && (p.AddressFamily == "ipv4" || p.AddressFamily == "ipv6")
}
func ParseRoute(route string) (int, error) {
	if !strings.HasPrefix(route, "tcp:") {
		return 0, ErrInvalid
	}
	n, e := strconv.Atoi(strings.TrimPrefix(route, "tcp:"))
	if e != nil || n < 1 || n > 65535 || route != "tcp:"+strconv.Itoa(n) {
		return 0, ErrInvalid
	}
	return n, nil
}
func ValidateSnapshot(in Snapshot, now time.Time) error {
	if in.Schema != "paperboat.machine-services/v1" || in.InstallationGeneration < 1 || in.Generation < 1 || in.Generation > 9007199254740991 || !ValidBoot(in.BootID) || in.StartedAt.IsZero() || in.StartedAt.After(now.Add(5*time.Second)) || len(in.Services) > MaxServices {
		return ErrInvalid
	}
	seen := map[int]bool{}
	for _, p := range in.Services {
		if !ValidPort(p) || seen[p.Port] {
			return ErrInvalid
		}
		seen[p.Port] = true
	}
	return nil
}

type Item struct {
	MachineID              string          `json:"machine_id"`
	Alias                  string          `json:"alias"`
	Description            string          `json:"description,omitempty"`
	InstallationGeneration int64           `json:"installation_generation"`
	BootID                 string          `json:"boot_id"`
	PolicyGeneration       int64           `json:"policy_generation"`
	AnnouncementGeneration int64           `json:"announcement_generation"`
	ExpiresAt              time.Time       `json:"expires_at"`
	Services               []PublishedPort `json:"services"`
}

func ValidBoot(s string) bool {
	if len(s) < 16 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r != '-' && r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

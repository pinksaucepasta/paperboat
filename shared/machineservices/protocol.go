// Package machineservices defines machine-service discovery and policy wire contracts.
package machineservices

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
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
}
type PolicyUpdate struct {
	ExpectedGeneration  int64 `json:"expected_generation"`
	PersonalAutoEnabled bool  `json:"personal_auto_enabled"`
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
	if in.Schema != "paperboat.machine-services/v1" || in.InstallationGeneration < 1 || in.Generation < 1 || in.Generation > 9007199254740991 || !ValidBoot(in.BootID) || in.StartedAt.IsZero() || in.StartedAt.After(now.Add(serverclock.Threshold)) || len(in.Services) > MaxServices {
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
	InstallationGeneration int64           `json:"installation_generation"`
	BootID                 string          `json:"boot_id"`
	PolicyGeneration       int64           `json:"policy_generation"`
	AnnouncementGeneration int64           `json:"announcement_generation"`
	ExpiresAt              time.Time       `json:"expires_at"`
	Services               []PublishedPort `json:"services"`
	// PortNames are the browser names of this machine's listed ports (<name>.<alias>.<domain>): machine names
	// first, then the workspace's all-machines names of another label. Names grant no access: only listed ports.
	PortNames []PortName `json:"port_names"`
	// TopLevelNames map <name>.<domain> to one of this machine's listed ports. A machine named <name> wins.
	TopLevelNames []PortName `json:"top_level_names"`
	FrontDesk     FrontDesk  `json:"front_desk"`
}

// Port name scopes.
const (
	ScopeMachine     = "machine"
	ScopeAllMachines = "all_machines"
	ScopeTopLevel    = "top_level"
)

// DefaultFrontDesk is where an unmatched <name>.<machine>.<domain> goes when the machine sets none.
var DefaultFrontDesk = FrontDesk{Port: 80, Scheme: "http"}

type PortName struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// FrontDesk is the port (and scheme) of a machine's reverse proxy, such as Coolify, Traefik or Caddy.
type FrontDesk struct {
	Port   int    `json:"port"`
	Scheme string `json:"scheme"`
}

// PortNameRecord is one stored name, as the management API lists it.
type PortNameRecord struct {
	ID           string `json:"id"`
	Scope        string `json:"scope"`
	Name         string `json:"name"`
	Port         int    `json:"port"`
	MachineID    string `json:"machine_id,omitempty"`
	MachineAlias string `json:"machine_alias,omitempty"`
	// Shadowed: a top-level name whose label is also a machine's name; <name>.<domain> opens that machine.
	Shadowed bool `json:"shadowed"`
}

type PortNameSet struct {
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Port      int    `json:"port"`
	MachineID string `json:"machine_id,omitempty"`
}

// ValidPortName: one lowercase DNS label that is not a number (numeric labels always mean the port).
func ValidPortName(name string) bool {
	if len(name) < 1 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	numeric := true
	for _, c := range name {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
		numeric = numeric && c >= '0' && c <= '9'
	}
	return !numeric
}

func ValidFrontDesk(f FrontDesk) bool {
	return f.Port >= 1 && f.Port <= 65535 && (f.Scheme == "http" || f.Scheme == "https")
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

package connectorprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WriteIngressDecision precedes application bytes on every replacement edge
// stream. Its bounded, strict encoding is shared by HTTP and opaque TCP.
func WriteIngressDecision(w io.Writer, d IngressDecision, now time.Time) error {
	if w == nil || d.Validate(now) != nil {
		return ErrIngressDenied
	}
	d.VisitorAssertion = "" // the app receives it as a request header; the agent never needs it
	p, err := json.Marshal(d)
	if err != nil || len(p) > MaxStreamOpenBytes {
		return ErrIngressDenied
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(p)))
	if _, err := writeAllStreamOpen(w, h[:]); err != nil {
		return err
	}
	_, err = writeAllStreamOpen(w, p)
	return err
}

func ReadIngressDecision(r io.Reader, now time.Time) (IngressDecision, error) {
	return readIngressDecision(r, now, false)
}

// ReadAdmittedIngressDecision decodes an authenticated current-authority reply for an existing
// admission (see ValidateAdmitted). A new application stream must always use ReadIngressDecision.
func ReadAdmittedIngressDecision(r io.Reader, now time.Time) (IngressDecision, error) {
	return readIngressDecision(r, now, true)
}

func readIngressDecision(r io.Reader, now time.Time, admitted bool) (IngressDecision, error) {
	var d IngressDecision
	if r == nil {
		return d, ErrIngressDenied
	}
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return d, ingressReadFailure{cause: err}
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > MaxStreamOpenBytes {
		return d, ErrIngressDenied
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return d, ingressReadFailure{cause: err}
	}
	if decodeStrict(p, &d) != nil || d.validate(now, admitted) != nil {
		return IngressDecision{}, ErrIngressDenied
	}
	return d, nil
}

const (
	IngressAuthorityLifetime = 10 * time.Second
	IngressRefreshInterval   = 5 * time.Second
	TunnelEphemeral          = "ephemeral"
	TunnelDurable            = "durable"
)

var ErrIngressDenied = errors.New("connector ingress authority denied")

// ErrIngressQuotaExhausted is the typed reason an edge refuses a new stream at admission (or
// ends one) because the publication's workspace has no bandwidth allowance left.
var ErrIngressQuotaExhausted = errors.New("workspace bandwidth allowance exhausted; retry after allowance recovery")

// IngressQuotaDenial names a public route the server withheld from the ingress snapshot only
// because its workspace is out of allowance, so the edge can answer visitors with the typed
// reason instead of a generic failure. It grants no authority.
type IngressQuotaDenial struct {
	RouteID  string `json:"route_id"`
	Hostname string `json:"hostname"`
	// ResetsAt is when the workspace's allowance period ends, so the visitor's page can say when
	// the address resumes; zero when the server does not know.
	ResetsAt time.Time `json:"resets_at,omitzero"`
}

// IngressBlock names a hostname an operator took down for violating the acceptable use policy. The
// server withholds its decisions; the edge answers its fixed takedown page (451 when legal, else
// 403) without contacting the origin. It grants no authority.
type IngressBlock struct {
	Hostname string `json:"hostname"`
	Legal    bool   `json:"legal,omitempty"`
}

// IngressIPRule is the owner's IP allow and deny lists (CIDRs) for one hostname. The edge refuses a
// client address a deny entry matches, or that no allow entry matches when allow is not empty, before
// opening the origin. It grants no authority.
type IngressIPRule struct {
	Hostname string   `json:"hostname"`
	Allow    []string `json:"allow,omitempty"`
	Deny     []string `json:"deny,omitempty"`
}

// ErrPublicationBlocked is the typed reason an edge refuses a taken-down hostname.
var ErrPublicationBlocked = errors.New("publication taken down for violating the acceptable use policy")

// PublicationBlockedError is ErrPublicationBlocked with whether the takedown is legal (451).
type PublicationBlockedError struct{ Legal bool }

func (e PublicationBlockedError) Error() string { return ErrPublicationBlocked.Error() }
func (e PublicationBlockedError) Unwrap() error { return ErrPublicationBlocked }

// IngressQuotaError is ErrIngressQuotaExhausted with the denial's reset time.
type IngressQuotaError struct{ ResetsAt time.Time }

func (e IngressQuotaError) Error() string { return ErrIngressQuotaExhausted.Error() }
func (e IngressQuotaError) Unwrap() error { return ErrIngressQuotaExhausted }

type ingressReadFailure struct{ cause error }

func (ingressReadFailure) Error() string { return ErrIngressDenied.Error() }

func (e ingressReadFailure) Unwrap() []error { return []error{ErrIngressDenied, e.cause} }

// IngressBinding is the single v1 tunnel route/target identity shared by both
// lifecycle modes. It is issued by the server, never inferred from reachability
// or public request headers. Native private HTTP uses the same independently revalidated edge bindings.
type IngressBinding struct {
	EnvironmentID           string `json:"environment_id"`
	AccountID               string `json:"account_id"`
	TunnelID                string `json:"tunnel_id"`
	Lifecycle               string `json:"lifecycle"`
	ResourceGeneration      uint64 `json:"resource_generation"`
	RouteID                 string `json:"route_id"`
	RouteGeneration         uint64 `json:"route_generation"`
	TargetID                string `json:"target_id"`
	TargetGeneration        uint64 `json:"target_generation"`
	HostID                  string `json:"host_id"`
	InstallationGeneration  uint64 `json:"installation_generation"`
	Audience                string `json:"audience"`
	ConnectionMethod        string `json:"connection_method"`
	Protocol                string `json:"protocol"`
	Hostname                string `json:"hostname"`
	PathPrefix              string `json:"path_prefix"`
	OriginScheme            string `json:"origin_scheme"`
	OriginAddress           string `json:"origin_address"`
	TLSServerName           string `json:"tls_server_name"`
	TLSVerification         string `json:"tls_verification"`
	CAReference             string `json:"ca_reference"`
	MTLSCredentialReference string `json:"mtls_credential_reference"`
	TLSPin                  string `json:"tls_pin,omitempty"`
	PublicationID           string `json:"publication_id"`
	PublicationGeneration   uint64 `json:"publication_generation"`
	ListenerID              string `json:"listener_id"`
	PublicPort              uint16 `json:"public_port"`
}

func (b IngressBinding) Validate() error {
	for _, id := range []string{b.EnvironmentID, b.AccountID, b.TunnelID, b.RouteID, b.TargetID, b.HostID, b.PublicationID} {
		if ValidateIdentifier(id) != nil {
			return ErrIngressDenied
		}
	}
	if (b.Lifecycle != TunnelEphemeral && b.Lifecycle != TunnelDurable) || b.ConnectionMethod != "edge" ||
		b.ResourceGeneration == 0 || b.RouteGeneration == 0 || b.TargetGeneration == 0 || b.InstallationGeneration == 0 || b.PublicationGeneration == 0 {
		return ErrIngressDenied
	}
	if b.Audience != "public" && b.Audience != "private" && b.Audience != "team" {
		return ErrIngressDenied
	}
	if b.OriginScheme == "unix" {
		if b.Protocol != "http" || len(b.OriginAddress) > 512 || !path.IsAbs(b.OriginAddress) || path.Clean(b.OriginAddress) != b.OriginAddress || strings.ContainsAny(b.OriginAddress, "\x00\r\n") {
			return ErrIngressDenied
		}
	} else if b.OriginScheme == "file" {
		// One Inbox file id on the host (16 lowercase hex); the host resolves it in its Inbox folder.
		if b.Protocol != "http" || len(b.OriginAddress) != 16 || strings.Trim(b.OriginAddress, "0123456789abcdef") != "" {
			return ErrIngressDenied
		}
	} else {
		host, port, err := net.SplitHostPort(b.OriginAddress)
		ip := net.ParseIP(host)
		n, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || n < 1 || n > 65535 || ip == nil || !ip.IsLoopback() {
			return ErrIngressDenied
		}
	}
	if b.Hostname == "" || len(b.Hostname) > 253 || b.Hostname != strings.ToLower(b.Hostname) || strings.ContainsAny(b.Hostname, " /\\:@\r\n") {
		return ErrIngressDenied
	}
	switch b.Protocol {
	case "http":
		if b.ListenerID != "" || b.PublicPort != 0 || !strings.HasPrefix(b.PathPrefix, "/") || strings.ContainsAny(b.PathPrefix, "?#\r\n") {
			return ErrIngressDenied
		}
		if b.OriginScheme != "http" && b.OriginScheme != "https" && b.OriginScheme != "h2c" && b.OriginScheme != "unix" && b.OriginScheme != "file" {
			return ErrIngressDenied
		}
	case "tcp", "tls":
		if b.Protocol == "tls" && (b.ListenerID != "listener_tls_443" || b.PublicPort != 443) {
			return ErrIngressDenied
		}
		if b.Audience != "public" || ValidateIdentifier(b.ListenerID) != nil || b.PublicPort == 0 || b.PathPrefix != "" || b.OriginScheme != "tcp" {
			return ErrIngressDenied
		}
	default:
		return ErrIngressDenied
	}
	if b.OriginScheme == "https" {
		if b.TLSVerification != "system" && b.TLSVerification != "custom_ca" && b.TLSVerification != "insecure" && b.TLSVerification != "pinned" {
			return ErrIngressDenied
		}
		if (b.TLSVerification == "pinned") != (b.TLSPin != "") {
			return ErrIngressDenied
		}
		if b.TLSVerification == "custom_ca" && b.CAReference == "" {
			return ErrIngressDenied
		}
	} else if b.TLSVerification != "not_applicable" || b.TLSServerName != "" || b.CAReference != "" || b.MTLSCredentialReference != "" || b.TLSPin != "" {
		return ErrIngressDenied
	}
	return nil
}

// IngressDecision is typed provenance over the authenticated connector. Public
// publication is owner authority; restricted viewer grants are separate fields.
// A daemon compares Binding to its independently obtained current server binding.
type IngressDecision struct {
	NativeAuthorization  *PrivateAccessOpen `json:"native_authorization,omitempty"`
	Binding              IngressBinding     `json:"binding"`
	DecisionID           string             `json:"decision_id"`
	PolicyGeneration     uint64             `json:"policy_generation"`
	EdgeNodeID           string             `json:"edge_node_id"`
	EdgeProcessEpoch     string             `json:"edge_process_epoch"`
	ConnectorID          string             `json:"connector_id"`
	SessionID            string             `json:"session_id"`
	ProcessGeneration    uint64             `json:"process_generation"`
	ConfigGeneration     uint64             `json:"config_generation"`
	AssignmentGeneration uint64             `json:"assignment_generation"`
	PrincipalID          string             `json:"principal_id"`
	GrantID              string             `json:"grant_id"`
	GrantGeneration      uint64             `json:"grant_generation"`
	MembershipGeneration uint64             `json:"membership_generation"`
	Action               string             `json:"action"`
	IssuedAt             time.Time          `json:"issued_at"`
	ExpiresAt            time.Time          `json:"expires_at"`
	// VisitorAssertion is the signed visitor identity (paperboat-visitor+jwt) the edge hands a gated
	// route's app; it grants nothing to Paperboat itself.
	VisitorAssertion string `json:"visitor_assertion,omitempty"`
}

func (d IngressDecision) Validate(now time.Time) error { return d.validate(now, false) }

// ValidateAdmitted checks an existing admission's exact shape without using its
// credential deadline as a live-session deadline.
func (d IngressDecision) ValidateAdmitted(now time.Time) error { return d.validate(now, true) }

func (d IngressDecision) validate(now time.Time, admitted bool) error {
	if d.NativeAuthorization != nil && d.validateNativeAuthorization(now, admitted) != nil {
		return ErrIngressDenied
	}
	if d.Binding.Validate() != nil || now.IsZero() || d.IssuedAt.IsZero() || d.IssuedAt.After(now.Add(MaxClockSkew)) || !d.ExpiresAt.After(d.IssuedAt) || !admitted && !d.ExpiresAt.After(now) || !d.ExpiresAt.After(d.IssuedAt) || d.ExpiresAt.Sub(d.IssuedAt) > IngressAuthorityLifetime || d.Action != "view" {
		return ErrIngressDenied
	}
	for _, id := range []string{d.DecisionID, d.EdgeNodeID, d.ConnectorID, d.SessionID} {
		if ValidateIdentifier(id) != nil {
			return ErrIngressDenied
		}
	}
	if ValidateOpaqueEpoch(d.EdgeProcessEpoch) != nil || d.PolicyGeneration == 0 || d.ProcessGeneration == 0 || d.ConfigGeneration == 0 || d.AssignmentGeneration == 0 {
		return ErrIngressDenied
	}
	if d.Binding.Audience == "public" {
		if d.PrincipalID != "" || d.GrantID != "" || d.GrantGeneration != 0 || d.MembershipGeneration != 0 {
			return ErrIngressDenied
		}
	} else {
		if ValidateIdentifier(d.PrincipalID) != nil || ValidateIdentifier(d.GrantID) != nil || d.GrantGeneration == 0 {
			return ErrIngressDenied
		}
		if d.Binding.Audience == "team" && d.MembershipGeneration == 0 {
			return ErrIngressDenied
		}
	}
	return nil
}

func (d IngressDecision) Authorize(authority IngressDecision, open StreamOpen, edgeNodeID, edgeEpoch string, now time.Time) error {
	return d.authorize(authority, open, edgeNodeID, edgeEpoch, now, false)
}

// Revalidate checks exact current revocation authority for already admitted work.
// It cannot be used to admit a new stream.
func (d IngressDecision) Revalidate(authority IngressDecision, open StreamOpen, edgeNodeID, edgeEpoch string, now time.Time) error {
	return d.authorize(authority, open, edgeNodeID, edgeEpoch, now, true)
}

func (d IngressDecision) authorize(authority IngressDecision, open StreamOpen, edgeNodeID, edgeEpoch string, now time.Time, admitted bool) error {
	current := authority.Binding
	if d.validate(now, admitted) != nil || authority.validate(now, admitted) != nil || d.Binding != current || open.Validate() != nil ||
		d.PolicyGeneration != authority.PolicyGeneration || d.AssignmentGeneration != authority.AssignmentGeneration ||
		d.ConnectorID != authority.ConnectorID || d.SessionID != authority.SessionID || d.ProcessGeneration != authority.ProcessGeneration || d.ConfigGeneration != authority.ConfigGeneration ||
		d.EdgeNodeID != authority.EdgeNodeID || d.EdgeProcessEpoch != authority.EdgeProcessEpoch ||
		!sameNativeAuthorization(d.NativeAuthorization, authority.NativeAuthorization) || d.PrincipalID != authority.PrincipalID || d.GrantID != authority.GrantID || d.GrantGeneration != authority.GrantGeneration || d.MembershipGeneration != authority.MembershipGeneration ||
		open.AccountID != current.AccountID || open.TunnelID != current.TunnelID || open.RouteID != current.RouteID ||
		d.ConnectorID != open.ConnectorID || d.SessionID != open.SessionID || d.ProcessGeneration != open.ProcessGeneration || d.ConfigGeneration != open.Generation ||
		d.EdgeNodeID != edgeNodeID || d.EdgeProcessEpoch != edgeEpoch {
		return ErrIngressDenied
	}
	return nil
}

// ValidateNativeAuthorization binds the signed accessor authority to the exact
// durable publisher assignment. Publisher identity is independently projected.
func (d IngressDecision) ValidateNativeAuthorization(now time.Time) error {
	return d.validateNativeAuthorization(now, false)
}

func (d IngressDecision) validateNativeAuthorization(now time.Time, admitted bool) error {
	a := d.NativeAuthorization
	validationTime := now
	if admitted {
		validationTime = time.Time{}
	}
	if a == nil || a.Validate(validationTime) != nil {
		return ErrIngressDenied
	}
	q := a.Request
	b := d.Binding
	resourceMatches := b.Lifecycle == TunnelDurable && q.ResourceKind == "tunnel" && q.ResourceID == b.TunnelID && q.Audience == "paperboat-tunnel-http"
	if b.Lifecycle == TunnelEphemeral {
		resourceMatches = q.ResourceKind == "preview" && q.ResourceID == b.PublicationID && ValidateIdentifier(q.OperationID) == nil && q.Audience == "paperboat-preview-http"
	}
	connectorMatches := q.ConnectorID == d.ConnectorID
	if b.Lifecycle == TunnelEphemeral {
		connectorMatches = q.ConnectorID == ""
	}
	if !resourceMatches || !connectorMatches || b.Audience != "private" || b.Protocol != "http" || q.Protocol != "http" ||
		q.AccountID != b.AccountID || q.RouteID != b.RouteID || q.Host != b.Hostname || q.RouteGeneration != b.RouteGeneration ||
		q.CarrierSessionID != d.SessionID || q.ProcessGeneration != d.ProcessGeneration || q.ConfigGeneration != d.ConfigGeneration || q.AssignmentGeneration != d.AssignmentGeneration ||
		q.EdgeNodeID != d.EdgeNodeID || q.EdgeProcessEpoch != d.EdgeProcessEpoch || d.PrincipalID != q.MachineID || d.GrantID != q.Nonce || d.GrantGeneration != q.InstallationGeneration || d.MembershipGeneration != 0 || !admitted && d.ExpiresAt.After(q.ExpiresAt) {
		return ErrIngressDenied
	}
	// Keep the combined envelope within the existing stream admission bound.
	encoded, err := json.Marshal(d)
	if err != nil || len(encoded) > MaxStreamOpenBytes {
		return ErrIngressDenied
	}
	return nil
}
func sameNativeAuthorization(a, b *PrivateAccessOpen) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// IngressVersion orders decisions, quota denials and http-01 responses by their stable form (the
// decision id and its issue and expiry times are per-answer values) and returns a version of the
// whole answer. An edge that sends the version it holds gets only fresh times when nothing else
// changed (F21-38). Prior art: cloudflared applies remote config only on a version change.
func IngressVersion(decisions []IngressDecision, denied []IngressQuotaDenial, blocked []IngressBlock, ipRules []IngressIPRule, http01 []HTTP01Response) string {
	stable := make([][]byte, len(decisions))
	order := make([]int, len(decisions))
	for i, d := range decisions {
		d.DecisionID, d.IssuedAt, d.ExpiresAt = "", time.Time{}, time.Time{}
		stable[i], _ = json.Marshal(d)
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return bytes.Compare(stable[order[a]], stable[order[b]]) < 0 })
	sorted := make([]IngressDecision, len(decisions))
	hash := sha256.New()
	for position, i := range order {
		sorted[position] = decisions[i]
		hash.Write(stable[i])
		hash.Write([]byte{'\n'})
	}
	copy(decisions, sorted)
	sort.Slice(denied, func(a, b int) bool {
		return denied[a].RouteID+" "+denied[a].Hostname < denied[b].RouteID+" "+denied[b].Hostname
	})
	sort.Slice(blocked, func(a, b int) bool { return blocked[a].Hostname < blocked[b].Hostname })
	sort.Slice(ipRules, func(a, b int) bool { return ipRules[a].Hostname < ipRules[b].Hostname })
	sort.Slice(http01, func(a, b int) bool { return http01[a].Token < http01[b].Token })
	rest, _ := json.Marshal([]any{denied, blocked, ipRules, http01})
	hash.Write(rest)
	return hex.EncodeToString(hash.Sum(nil))
}

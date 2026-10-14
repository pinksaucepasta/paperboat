package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/mail"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"golang.org/x/net/idna"
)

const terminalBrowserAccessEmailLimit = 100

var (
	ErrTerminalBrowserPolicyInvalid                   = errors.New("terminal browser access policy is invalid")
	ErrTerminalBrowserPolicyConflict                  = errors.New("terminal browser access policy changed; refresh before retrying")
	ErrTerminalBrowserInteractionConfirmationRequired = errors.New("interactive terminal access requires explicit confirmation")
	ErrTerminalBrowserSessionClosed                   = errors.New("terminal session is closed or unavailable")
)

type TerminalBrowserRole string

const (
	TerminalBrowserRoleOwner       TerminalBrowserRole = "owner"
	TerminalBrowserRoleViewer      TerminalBrowserRole = "viewer"
	TerminalBrowserRoleInteractive TerminalBrowserRole = "interactive"
)

// TerminalBrowserAccessPolicy is owner-managed browser access for one exact
// terminal. It grants no team, machine, ENV, SSH, or native access.
type TerminalBrowserAccessPolicy struct {
	TerminalSessionID string    `json:"terminal_session_id"`
	Audience          string    `json:"audience"`
	TeamID            string    `json:"team_id,omitempty"`
	AllowedEmails     []string  `json:"allowed_emails"`
	InteractiveEmails []string  `json:"interactive_emails"`
	Generation        uint64    `json:"generation"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type TerminalBrowserAccessPolicyMutation struct {
	ExpectedGeneration       uint64   `json:"expected_generation"`
	Audience                 string   `json:"audience"`
	TeamID                   string   `json:"team_id,omitempty"`
	AllowedEmails            []string `json:"allowed_emails,omitempty"`
	InteractiveEmails        []string `json:"interactive_emails,omitempty"`
	ConfirmInteractiveAccess bool     `json:"confirm_interactive_access"`
}

// TerminalBrowserDecision is an exact, current server decision for a browser
// attachment. ProcessGeneration is deliberately absent: the control plane
// must fetch it from the current host snapshot before minting a host token.
type TerminalBrowserDecision struct {
	GuestSessionID       string              `json:"-"`
	AccountID            string              `json:"account_id"`
	OwnerAccountID       string              `json:"owner_account_id"`
	WorkspaceID          string              `json:"workspace_id"`
	SessionID            string              `json:"terminal_session_id"`
	MachineID            string              `json:"machine_id"`
	EnvironmentID        string              `json:"environment_id"`
	HostWSSURL           string              `json:"host_wss_url,omitempty"`
	EdgeNodeID           string              `json:"-"`
	EdgeProcessEpoch     string              `json:"-"`
	EdgeEndpointHost     string              `json:"-"`
	Role                 TerminalBrowserRole `json:"role"`
	PolicyGeneration     uint64              `json:"policy_generation"`
	TeamID               string              `json:"team_id,omitempty"`
	TeamGeneration       uint64              `json:"team_generation,omitempty"`
	MembershipGeneration uint64              `json:"membership_generation,omitempty"`
	BindingGeneration    uint64              `json:"binding_generation,omitempty"`
}

// TerminalBrowserRuntimeSnapshot is returned only after rechecking the
// account's current policy. Generation is the authoritative host PTY process
// generation used when a future gateway issues browser_terminal_operation.
type TerminalBrowserRuntimeSnapshot struct {
	State      string                 `json:"state"`
	Generation uint64                 `json:"generation"`
	Snapshot   helperruntime.Snapshot `json:"snapshot"`
}

func (s *Service) TerminalBrowserAccessPolicy(ctx context.Context, accountID, sessionID string) (TerminalBrowserAccessPolicy, error) {
	var policy TerminalBrowserAccessPolicy
	err := s.db.Pool().QueryRow(ctx, `
SELECT p.terminal_session_id,p.audience,coalesce(p.team_id,''),p.allowed_emails,
       p.interactive_emails,p.generation,p.updated_at
FROM terminal_browser_access_policies p
JOIN user_machine_terminal_sessions s ON s.id=p.terminal_session_id
WHERE p.terminal_session_id=$1 AND s.owner_account=$2 AND s.deleted_at IS NULL`, sessionID, accountID).
		Scan(&policy.TerminalSessionID, &policy.Audience, &policy.TeamID, &policy.AllowedEmails,
			&policy.InteractiveEmails, &policy.Generation, &policy.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
		return TerminalBrowserAccessPolicy{}, ErrTerminalSessionNotFound
	}
	if err != nil {
		return TerminalBrowserAccessPolicy{}, err
	}
	if policy.AllowedEmails == nil {
		policy.AllowedEmails = []string{}
	}
	if policy.InteractiveEmails == nil {
		policy.InteractiveEmails = []string{}
	}
	return policy, nil
}

func (s *Service) SetTerminalBrowserAccessPolicy(ctx context.Context, accountID, sessionID string, mutation TerminalBrowserAccessPolicyMutation) (TerminalBrowserAccessPolicy, error) {
	requested, err := normalizeTerminalBrowserPolicy(sessionID, mutation)
	if err != nil {
		return TerminalBrowserAccessPolicy{}, err
	}
	var saved TerminalBrowserAccessPolicy
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_account FROM user_machine_terminal_sessions WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, sessionID).Scan(&ownerID); errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
			return ErrTerminalSessionNotFound
		} else if err != nil {
			return err
		}
		if ownerID != accountID {
			return ErrTerminalSessionNotFound
		}

		current, err := readTerminalBrowserPolicyTx(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if mutation.ExpectedGeneration != current.Generation {
			return ErrTerminalBrowserPolicyConflict
		}
		if requested.Audience == "team" {
			var associated bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM teams t
 JOIN team_members owner_member ON owner_member.team_id=t.team_id AND owner_member.account_id=$3 AND owner_member.active
 WHERE t.team_id=$1 AND t.deleted_at IS NULL AND NOT t.suspended AND team_paid_entitled(t.team_id)
 AND EXISTS(SELECT 1 FROM user_machine_terminal_sessions target WHERE target.id=$2 AND target.owner_account=$3 AND target.deleted_at IS NULL)
)`, requested.TeamID, sessionID, accountID).Scan(&associated); err != nil {
				return err
			}
			if !associated {
				return ErrTerminalSessionNotFound
			}
		}

		addsInteractive := false
		for _, email := range requested.InteractiveEmails {
			if !slices.Contains(current.InteractiveEmails, email) {
				addsInteractive = true
				break
			}
		}
		if addsInteractive && !mutation.ConfirmInteractiveAccess {
			return ErrTerminalBrowserInteractionConfirmationRequired
		}
		if terminalBrowserPoliciesEqual(current, requested) {
			saved = current
			return nil
		}
		if current.Generation >= 9007199254740991 {
			return ErrTerminalBrowserPolicyConflict
		}
		generation := current.Generation + 1
		if _, err = tx.Exec(ctx, `UPDATE terminal_browser_access_policies
SET audience=$2,team_id=nullif($3,''),allowed_emails=$4,
    interactive_emails=$5,generation=$6,updated_at=now()
WHERE terminal_session_id=$1 AND generation=$7`, sessionID, requested.Audience, requested.TeamID,
			requested.AllowedEmails, requested.InteractiveEmails, generation, current.Generation); err != nil {
			return err
		}
		saved = requested
		saved.Generation = generation
		saved.UpdatedAt = s.now().UTC()
		if s.audit != nil {
			if err = s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: accountID, ActorType: audit.ActorUser,
				EventType: "terminal.browser_access_policy_changed", ResourceType: "terminal_session", ResourceID: sessionID,
				Metadata: map[string]any{"audience": requested.Audience, "generation": generation,
					"allowed_email_count": len(requested.AllowedEmails), "interactive_email_count": len(requested.InteractiveEmails)}}); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, ErrTerminalSessionNotFound) || errors.Is(err, ErrTerminalBrowserPolicyConflict) || errors.Is(err, ErrTerminalBrowserInteractionConfirmationRequired) || errors.Is(err, ErrTerminalBrowserPolicyInvalid) {
		return TerminalBrowserAccessPolicy{}, err
	}
	if err != nil {
		return TerminalBrowserAccessPolicy{}, err
	}
	return saved, nil
}

func (s *Service) ResolveTerminalBrowserRole(ctx context.Context, accountID, sessionID string) (TerminalBrowserDecision, error) {
	var decision TerminalBrowserDecision
	var actorEmail, audience, desiredState, runtimeState string
	var actorVerified, actorIsOwner, member, teamPaid, ownerMember bool
	var allowed, interactive []string
	err := s.db.Pool().QueryRow(ctx, `
SELECT s.id,s.owner_account,coalesce(s.owner_team_id,'personal'),m.id,m.environment_id,s.desired_state,s.runtime_state,
       actor.primary_email,actor.email_verified,
       p.audience,coalesce(p.team_id,''),p.allowed_emails,p.interactive_emails,p.generation,
       (s.owner_account=actor.id),
       coalesce(tm.active,false),coalesce(team_paid_entitled(p.team_id),false),coalesce(t.generation,0),coalesce(tm.membership_generation,0),coalesce(b.generation,0),coalesce(owner_member.active,false)
FROM user_machine_terminal_sessions s
JOIN user_machines m ON m.id=s.user_machine_id AND m.deleted_at IS NULL AND m.revoked_at IS NULL
JOIN users owner ON owner.id=s.owner_account AND owner.status='active'
JOIN users actor ON actor.id=$2 AND actor.status='active'
JOIN terminal_browser_access_policies p ON p.terminal_session_id=s.id
LEFT JOIN teams t ON t.team_id=p.team_id AND t.deleted_at IS NULL AND NOT t.suspended
LEFT JOIN team_members tm ON tm.team_id=p.team_id AND tm.account_id=actor.id AND tm.active
LEFT JOIN team_members owner_member ON owner_member.team_id=p.team_id AND owner_member.account_id=s.owner_account AND owner_member.active
LEFT JOIN team_resource_bindings b ON b.team_id=p.team_id AND b.resource_kind='terminal_session' AND b.resource_id=s.id AND b.owner_account=s.owner_account AND b.active
WHERE s.id=$1 AND s.deleted_at IS NULL`, sessionID, accountID).Scan(
		&decision.SessionID, &decision.OwnerAccountID, &decision.WorkspaceID, &decision.MachineID, &decision.EnvironmentID, &desiredState, &runtimeState,
		&actorEmail, &actorVerified, &audience, &decision.TeamID, &allowed, &interactive, &decision.PolicyGeneration,
		&actorIsOwner, &member, &teamPaid, &decision.TeamGeneration, &decision.MembershipGeneration, &decision.BindingGeneration, &ownerMember)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
		return TerminalBrowserDecision{}, ErrTerminalSessionNotFound
	}
	if err != nil {
		return TerminalBrowserDecision{}, err
	}
	decision.AccountID = accountID
	if actorIsOwner {
		decision.Role = TerminalBrowserRoleOwner
	} else {
		verifiedEmail, emailErr := normalizeVerifiedWorkOSEmail(actorEmail)
		if audience == "team" && member && ownerMember && teamPaid && decision.TeamID != "" && decision.TeamGeneration > 0 {
			decision.Role = TerminalBrowserRoleViewer
		} else if actorVerified && emailErr == nil {
			switch audience {
			case "specific_emails":
				if terminalBrowserEmailAllowed(allowed, verifiedEmail) {
					decision.Role = TerminalBrowserRoleViewer
				}
			}
		}
		if decision.Role == TerminalBrowserRoleViewer && actorVerified && emailErr == nil && slices.Contains(interactive, verifiedEmail) {
			decision.Role = TerminalBrowserRoleInteractive
		}
		if decision.Role == "" {
			return TerminalBrowserDecision{}, ErrTerminalSessionNotFound
		}
	}

	if desiredState != "open" || terminalSessionRuntimeClosed(runtimeState) || runtimeState == "closing" || runtimeState == "failed" || runtimeState == "close_failed" {
		if actorIsOwner {
			return TerminalBrowserDecision{}, ErrTerminalBrowserSessionClosed
		}
		return TerminalBrowserDecision{}, ErrTerminalSessionNotFound
	}
	return s.completeTerminalBrowserRoute(ctx, decision)
}

func (s *Service) completeTerminalBrowserRoute(ctx context.Context, decision TerminalBrowserDecision) (TerminalBrowserDecision, error) {
	// The process generation comes from a fresh host snapshot, not the team
	// binding's cached generation. The current helper route is included so the
	// browser gateway can reach the same authenticated host endpoint.
	if route, routeErr := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, decision.EnvironmentID); routeErr == nil {
		httpIngressURL, ingressErr := s.runtimeIngressURL(ctx, decision.EnvironmentID, route.PublicHost)
		if ingressErr != nil {
			return TerminalBrowserDecision{}, ingressErr
		}
		decision.HostWSSURL = runtimeWebsocketBase(httpIngressURL) + "/v1/browser-terminal"
		if !route.AppliedNodeID.Valid || route.AppliedNodeID.String == "" {
			return TerminalBrowserDecision{}, ErrTerminalBrowserSessionClosed
		}
		decision.EdgeNodeID = route.AppliedNodeID.String
		if err := s.db.Pool().QueryRow(ctx, `SELECT process_epoch,endpoint_host FROM paperboat.control_tunnel_nodes WHERE id=$1 AND state='ready' AND ready=true AND public_ingress_verified_at IS NOT NULL`, decision.EdgeNodeID).Scan(&decision.EdgeProcessEpoch, &decision.EdgeEndpointHost); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
				return TerminalBrowserDecision{}, ErrTerminalBrowserSessionClosed
			}
			return TerminalBrowserDecision{}, err
		}
	} else if !errors.Is(routeErr, pgx.ErrNoRows) && !errors.Is(routeErr, sql.ErrNoRows) {
		return TerminalBrowserDecision{}, routeErr
	}
	return decision, nil
}

// SnapshotTerminalForBrowser rechecks the account policy before and after the
// host request, then returns the host's current process generation. The token
// used for this internal read is the control plane's owner-scoped snapshot
// credential; it is never a browser identity and is not returned.
func (s *Service) SnapshotTerminalForBrowser(ctx context.Context, want TerminalBrowserDecision) (TerminalBrowserRuntimeSnapshot, error) {
	current, err := s.resolveTerminalBrowserDecision(ctx, want)
	if err != nil || !sameTerminalBrowserDecision(want, current) {
		if err != nil {
			return TerminalBrowserRuntimeSnapshot{}, err
		}
		return TerminalBrowserRuntimeSnapshot{}, ErrTerminalSessionNotFound
	}
	if s.controlSigner == nil || s.controlRuntime == nil || current.HostWSSURL == "" {
		return TerminalBrowserRuntimeSnapshot{}, ErrTerminalBrowserSessionClosed
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, current.EnvironmentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
			return TerminalBrowserRuntimeSnapshot{}, ErrTerminalBrowserSessionClosed
		}
		return TerminalBrowserRuntimeSnapshot{}, err
	}
	now := s.now().UTC()
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{
		Issuer: s.issuer, Audience: "paperboat-machine", Subject: current.OwnerAccountID,
		JTI: newID("token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"},
		EnvironmentID: current.EnvironmentID, MachineID: current.MachineID,
		UserID: current.OwnerAccountID, CLIClientSessionID: "control-plane", SessionID: current.SessionID,
	})
	if err != nil {
		return TerminalBrowserRuntimeSnapshot{}, err
	}
	snapshot, err := s.controlRuntime.TerminalViaEdge(ctx, current.EdgeEndpointHost, route.PublicHost, token, "snapshot", current.SessionID, newID("operation"))
	if err != nil {
		return TerminalBrowserRuntimeSnapshot{}, err
	}
	latest, err := s.resolveTerminalBrowserDecision(ctx, want)
	if err != nil || !sameTerminalBrowserDecision(want, latest) {
		if err != nil {
			return TerminalBrowserRuntimeSnapshot{}, err
		}
		return TerminalBrowserRuntimeSnapshot{}, ErrTerminalSessionNotFound
	}
	if snapshot.State == "closed" || snapshot.State == "exited" || snapshot.State == "deleted" {
		return TerminalBrowserRuntimeSnapshot{State: snapshot.State, Generation: snapshot.Generation, Snapshot: snapshot}, ErrTerminalBrowserSessionClosed
	}
	if snapshot.Generation == 0 {
		return TerminalBrowserRuntimeSnapshot{}, fmt.Errorf("%w: host returned no process generation", ErrTerminalBrowserSessionClosed)
	}
	return TerminalBrowserRuntimeSnapshot{State: snapshot.State, Generation: snapshot.Generation, Snapshot: snapshot}, nil
}

func readTerminalBrowserPolicyTx(ctx context.Context, tx *db.Tx, sessionID string) (TerminalBrowserAccessPolicy, error) {
	var policy TerminalBrowserAccessPolicy
	err := tx.QueryRow(ctx, `SELECT terminal_session_id,audience,coalesce(team_id,''),allowed_emails,interactive_emails,generation,updated_at FROM terminal_browser_access_policies WHERE terminal_session_id=$1 FOR UPDATE`, sessionID).
		Scan(&policy.TerminalSessionID, &policy.Audience, &policy.TeamID, &policy.AllowedEmails,
			&policy.InteractiveEmails, &policy.Generation, &policy.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
		return TerminalBrowserAccessPolicy{}, ErrTerminalSessionNotFound
	}
	return policy, err
}

func normalizeTerminalBrowserPolicy(sessionID string, in TerminalBrowserAccessPolicyMutation) (TerminalBrowserAccessPolicy, error) {
	out := TerminalBrowserAccessPolicy{TerminalSessionID: sessionID, Audience: strings.TrimSpace(in.Audience),
		TeamID: strings.TrimSpace(in.TeamID)}
	var err error
	out.AllowedEmails, err = normalizeTerminalBrowserEmailPatterns(in.AllowedEmails)
	if err != nil {
		return TerminalBrowserAccessPolicy{}, err
	}
	out.InteractiveEmails, err = normalizeTerminalBrowserEmails(in.InteractiveEmails)
	if err != nil {
		return TerminalBrowserAccessPolicy{}, err
	}
	if len(out.AllowedEmails) > terminalBrowserAccessEmailLimit || len(out.InteractiveEmails) > terminalBrowserAccessEmailLimit {
		return TerminalBrowserAccessPolicy{}, ErrTerminalBrowserPolicyInvalid
	}

	switch out.Audience {
	case "owner":
		if out.TeamID != "" || len(out.AllowedEmails) > 0 || len(out.InteractiveEmails) > 0 {
			return TerminalBrowserAccessPolicy{}, ErrTerminalBrowserPolicyInvalid
		}
	case "team":
		if out.TeamID == "" || len(out.AllowedEmails) > 0 {
			return TerminalBrowserAccessPolicy{}, ErrTerminalBrowserPolicyInvalid
		}
	case "specific_emails":
		if out.TeamID != "" || len(out.AllowedEmails) == 0 {
			return TerminalBrowserAccessPolicy{}, ErrTerminalBrowserPolicyInvalid
		}
		for _, email := range out.InteractiveEmails {
			if !terminalBrowserEmailAllowed(out.AllowedEmails, email) {
				return TerminalBrowserAccessPolicy{}, ErrTerminalBrowserPolicyInvalid
			}
		}
	default:
		return TerminalBrowserAccessPolicy{}, ErrTerminalBrowserPolicyInvalid
	}
	return out, nil
}

func normalizeTerminalBrowserEmails(values []string) ([]string, error) {
	if len(values) > terminalBrowserAccessEmailLimit {
		return nil, ErrTerminalBrowserPolicyInvalid
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		email, err := normalizeVerifiedWorkOSEmail(value)
		if err != nil {
			return nil, ErrTerminalBrowserPolicyInvalid
		}
		if _, exists := seen[email]; exists {
			return nil, ErrTerminalBrowserPolicyInvalid
		}
		seen[email] = struct{}{}
		out = append(out, email)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeVerifiedWorkOSEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 || len(value) > 254 || strings.ContainsAny(value, "*?\r\n\x00") {
		return "", ErrTerminalBrowserPolicyInvalid
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value || strings.Count(value, "@") != 1 {
		return "", ErrTerminalBrowserPolicyInvalid
	}
	local, domain, found := strings.Cut(value, "@")
	if !found || local == "" || domain == "" || strings.ContainsAny(domain, "* /\\") {
		return "", ErrTerminalBrowserPolicyInvalid
	}
	asciiDomain, err := normalizeTerminalBrowserDomain(domain)
	if err != nil {
		return "", ErrTerminalBrowserPolicyInvalid
	}
	return local + "@" + asciiDomain, nil
}

func normalizeTerminalBrowserDomain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || strings.HasPrefix(value, "@") || strings.ContainsAny(value, "*@ /\\\r\n\x00") || strings.HasSuffix(value, ".") {
		return "", ErrTerminalBrowserPolicyInvalid
	}
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil {
		return "", ErrTerminalBrowserPolicyInvalid
	}
	ascii = strings.ToLower(ascii)
	if ascii == "" || len(ascii) > 253 || !strings.Contains(ascii, ".") {
		return "", ErrTerminalBrowserPolicyInvalid
	}
	labels := strings.Split(ascii, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrTerminalBrowserPolicyInvalid
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", ErrTerminalBrowserPolicyInvalid
			}
		}
	}
	return ascii, nil
}

func verifiedEmailHasExactDomain(email, domain string) bool {
	local, candidate, found := strings.Cut(email, "@")
	return found && local != "" && candidate == domain
}

func terminalBrowserPoliciesEqual(a, b TerminalBrowserAccessPolicy) bool {
	return a.Audience == b.Audience && a.TeamID == b.TeamID &&
		slices.Equal(a.AllowedEmails, b.AllowedEmails) && slices.Equal(a.InteractiveEmails, b.InteractiveEmails)
}

func sameTerminalBrowserDecision(want, got TerminalBrowserDecision) bool {
	return want.GuestSessionID == got.GuestSessionID && want.AccountID == got.AccountID && want.OwnerAccountID == got.OwnerAccountID &&
		want.WorkspaceID == got.WorkspaceID && want.SessionID == got.SessionID && want.Role == got.Role && want.PolicyGeneration == got.PolicyGeneration &&
		want.MachineID == got.MachineID && want.EnvironmentID == got.EnvironmentID &&
		want.HostWSSURL == got.HostWSSURL && want.TeamID == got.TeamID && want.TeamGeneration == got.TeamGeneration &&
		want.MembershipGeneration == got.MembershipGeneration && want.BindingGeneration == got.BindingGeneration
}

// A wildcard is only the entire local part. Domain matching is exact.
func normalizeTerminalBrowserEmailPatterns(values []string) ([]string, error) {
	if len(values) > terminalBrowserAccessEmailLimit {
		return nil, ErrTerminalBrowserPolicyInvalid
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		var normalized string
		var err error
		if strings.HasPrefix(value, "*@") {
			var domain string
			domain, err = normalizeTerminalBrowserDomain(strings.TrimPrefix(value, "*@"))
			normalized = "*@" + domain
		} else {
			normalized, err = normalizeVerifiedWorkOSEmail(value)
		}
		if err != nil || seen[normalized] {
			return nil, ErrTerminalBrowserPolicyInvalid
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out, nil
}
func terminalBrowserEmailAllowed(patterns []string, email string) bool {
	for _, pattern := range patterns {
		if pattern == email || strings.HasPrefix(pattern, "*@") && verifiedEmailHasExactDomain(email, strings.TrimPrefix(pattern, "*@")) {
			return true
		}
	}
	return false
}
func (s *Service) resolveTerminalBrowserDecision(ctx context.Context, want TerminalBrowserDecision) (TerminalBrowserDecision, error) {
	if want.GuestSessionID != "" {
		return s.ResolveTerminalGuestRole(ctx, want.GuestSessionID, want.SessionID)
	}
	return s.ResolveTerminalBrowserRole(ctx, want.AccountID, want.SessionID)
}

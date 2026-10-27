package localdaemon

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

type AuthenticatedMachineSource struct {
	ServerURL       string
	Auth            config.AuthSource
	SSHLocalReady   bool
	SSHLocalCode    string
	HTTPClient      *http.Client
	SourceMachineID string
	// AutoApprovePeerEnrollments is invoked after the authenticated machine
	// list is loaded. Production wires this to the account-root signer so a
	// one-shot enrollment never requires a separate trust-approval command.
	AutoApprovePeerEnrollments func(context.Context, *api.Client, []api.UserMachine) error
	// ReportPeerApprovalSignerUnavailable observes the one non-fatal approval
	// condition without making ordinary machine inventory unavailable.
	ReportPeerApprovalSignerUnavailable func(PeerApprovalSignerUnavailableError)
}

func (s *AuthenticatedMachineSource) SetManagedSSHReadiness(ready bool, code string) {
	if s == nil {
		return
	}
	s.SSHLocalReady = ready
	s.SSHLocalCode = code
}

// IssuePeerStream obtains a fresh, operation-scoped authorization just before
// transport admission. It deliberately never caches the returned credential.
func (s AuthenticatedMachineSource) IssuePeerStream(ctx context.Context, request localapi.PeerStreamRequest) (localapi.PeerStreamRequest, error) {
	if strings.TrimSpace(s.ServerURL) == "" || s.Auth == nil || request.Credential != "" || request.Offline {
		return request, ErrInvalidInventoryConfig
	}
	if err := api.ValidateWorkspaceSelector(request.Workspace); err != nil {
		return request, err
	}
	var expires time.Time
	var token string
	var quicEndpoint, wssEndpoint string
	if request.Consumer == "ssh" {
		// D1 (F5-07): the host admits every SSH stream from its own signed access rules, online or offline,
		// so an SSH connection makes no control-plane request.
		return hostRulesPeerStream(request)
	}
	if request.Consumer != "exec" {
		return request, errors.New("daemon descriptor issuance is unavailable for this consumer")
	}
	var descriptor api.ExecDescriptor
	for attempt := 0; attempt < 2; attempt++ {
		credential, err := s.Auth.Credential()
		if err != nil {
			return request, err
		}
		client := api.New(s.ServerURL, credential, s.HTTPClient)
		if err := client.SetWorkspace(request.Workspace); err != nil {
			return request, err
		}
		client.SetSourceMachineID(s.SourceMachineID)
		descriptor, err = client.MachineExecDescriptor(ctx, request.MachineID, request.OperationID)
		if err == nil {
			break
		}
		if attempt == 0 && errors.Is(err, api.ErrUnauthenticated) {
			if refresher, ok := s.Auth.(interface {
				Refresh() (config.Credential, error)
			}); ok {
				if _, refreshErr := refresher.Refresh(); refreshErr == nil {
					continue
				} else {
					return request, inventorySourceFailure("credential", errors.Join(err, refreshErr))
				}
			}
		}
		if api.Unreachable(err) {
			return hostRulesPeerStream(request)
		}
		return request, err
	}
	if descriptor.Environment == nil || descriptor.Environment.ID != request.EnvironmentID {
		return request, errors.New("operation descriptor environment mismatch")
	}
	token, expires = descriptor.Auth.Token, descriptor.Auth.ExpiresAt
	quicEndpoint, wssEndpoint = descriptor.Endpoints.QUIC, descriptor.Endpoints.WSS
	if token == "" || expires.IsZero() || !expires.After(time.Now()) {
		return request, errors.New("operation descriptor expired")
	}
	request.Credential, request.Deadline = token, expires.UTC()
	request.AccessSessionID = descriptor.Auth.AccessSessionID
	request.QUICEndpoint, request.WSSEndpoint = quicEndpoint, wssEndpoint
	if err := request.Validate(time.Now().UTC()); err != nil {
		return request, err
	}
	return request, nil
}

func (s AuthenticatedMachineSource) ListCompletionItems(ctx context.Context, machines []api.UserMachine) ([]localapi.CompletionItem, error) {
	const maximumCompletionItems = 1024
	if strings.TrimSpace(s.ServerURL) == "" || s.Auth == nil {
		return nil, ErrInvalidInventoryConfig
	}
	credential, err := s.Auth.Credential()
	if err != nil {
		return nil, err
	}
	client := api.New(s.ServerURL, credential, s.HTTPClient)
	items := make([]localapi.CompletionItem, 0, len(machines)*3)
	for _, machine := range machines {
		description := completionDescription(machine.Alias, machine.State)
		if machine.Alias != "" {
			items = append(items, localapi.CompletionItem{Kind: "machine", Value: machine.Alias, Description: description, EnvironmentID: machine.EnvironmentID})
		}
		items = append(items, localapi.CompletionItem{Kind: "machine", Value: machine.ID, Description: description, EnvironmentID: machine.EnvironmentID})
		if machine.Capabilities.FileReceive.Configured && machine.State != "revoked" && machine.State != "deleted" {
			if machine.Alias != "" {
				items = append(items, localapi.CompletionItem{Kind: "transfer_target", Value: machine.Alias, Description: description, EnvironmentID: machine.EnvironmentID})
			}
			items = append(items, localapi.CompletionItem{Kind: "transfer_target", Value: machine.ID, Description: description, EnvironmentID: machine.EnvironmentID})
		}
	}
	// Machine names complete even when the preview or session lists cannot be read: those lists are
	// only added to them. The failure is still returned for the refresh to report.
	var partial error
	previews, err := previewCompletionItems(ctx, client, machines, maximumCompletionItems-len(items))
	if err != nil {
		partial = err
	}
	items = append(items, previews...)
	// One catalog page for the caller's own sessions on every machine, stored rows only: listing
	// each machine separately cost N requests per daemon per refresh (O(N²) across an account).
	sessions, err := client.SharedTerminalSessionsFiltered(ctx, url.Values{"owner": {"mine"}, "live": {"false"}, "closed": {"false"}})
	if err != nil {
		partial = errors.Join(partial, inventorySourceFailure("completion_list", err))
	}
	environments := make(map[string]string, len(machines))
	for _, machine := range machines {
		environments[machine.ID] = machine.EnvironmentID
	}
	for _, session := range sessions {
		environment, listed := environments[session.Target.MachineID]
		if !listed {
			continue
		}
		description := completionDescription(session.Name, session.State)
		items = append(items, localapi.CompletionItem{Kind: "session", Value: session.ID, Description: description, EnvironmentID: environment})
		if session.Name != "" && session.Name != session.ID {
			items = append(items, localapi.CompletionItem{Kind: "session", Value: session.Name, Description: description, EnvironmentID: environment})
		}
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].Kind != items[right].Kind {
			return items[left].Kind < items[right].Kind
		}
		if items[left].EnvironmentID != items[right].EnvironmentID {
			return items[left].EnvironmentID < items[right].EnvironmentID
		}
		return items[left].Value < items[right].Value
	})
	if len(items) > maximumCompletionItems {
		items = items[:maximumCompletionItems]
	}
	return items, partial
}

func completionDescription(values ...string) string {
	result := strings.ToValidUTF8(strings.TrimSpace(strings.Join(values, " - ")), "?")
	result = strings.Map(func(character rune) rune {
		if character == '\x00' || character == '\t' || character == '\r' || character == '\n' {
			return ' '
		}
		return character
	}, result)
	if len(result) > 512 {
		result = result[:512]
		for !utf8.ValidString(result) {
			result = result[:len(result)-1]
		}
	}
	if result == "" {
		return "Paperboat resource"
	}
	return result
}

func (s AuthenticatedMachineSource) ListUserMachines(ctx context.Context) ([]api.UserMachine, error) {
	if strings.TrimSpace(s.ServerURL) == "" || s.Auth == nil {
		return nil, inventorySourceFailure("configuration", ErrInvalidInventoryConfig)
	}
	credential, err := s.Auth.Credential()
	if err != nil {
		return nil, inventorySourceFailure("credential", err)
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return nil, inventorySourceFailure("credential", api.ErrUnauthenticated)
	}
	client := api.New(s.ServerURL, credential, s.HTTPClient)
	machines, err := client.ListUserMachines(ctx)
	if errors.Is(err, api.ErrUnauthenticated) {
		if refresher, ok := s.Auth.(interface {
			Refresh() (config.Credential, error)
		}); ok {
			credential, refreshErr := refresher.Refresh()
			if refreshErr != nil {
				return nil, inventorySourceFailure("credential", refreshErr)
			}
			client = api.New(s.ServerURL, credential, s.HTTPClient)
			machines, err = client.ListUserMachines(ctx)
		}
	}
	if err != nil {
		return nil, inventorySourceFailure("machine_list", err)
	}
	if s.AutoApprovePeerEnrollments != nil {
		if err := s.AutoApprovePeerEnrollments(ctx, client, machines); err != nil {
			var unavailable *PeerApprovalSignerUnavailableError
			if !errors.As(err, &unavailable) {
				return nil, inventorySourceFailure("peer_approval", err)
			}
			if s.ReportPeerApprovalSignerUnavailable != nil {
				s.ReportPeerApprovalSignerUnavailable(*unavailable)
			}
		}
	}
	for index := range machines {
		if err := managedssh.ValidateAlias(machines[index].Alias); err != nil {
			return nil, inventorySourceFailure("machine_alias", err)
		}
		machines[index].SSHLocalReady = s.SSHLocalReady
		machines[index].SSHLocalCode = s.SSHLocalCode
	}
	return applyListedSSH(machines), nil
}

// RateLimitedPeerApprovalReporter bounds repeated diagnostics from inventory
// refreshes. The returned closure is concurrency-safe and reports immediately,
// then at most once per interval.
func RateLimitedPeerApprovalReporter(clock func() time.Time, interval time.Duration, report func(PeerApprovalSignerUnavailableError)) func(PeerApprovalSignerUnavailableError) {
	if clock == nil {
		clock = time.Now
	}
	if interval <= 0 {
		interval = time.Minute
	}
	var mu sync.Mutex
	var last time.Time
	return func(issue PeerApprovalSignerUnavailableError) {
		if report == nil {
			return
		}
		now := clock()
		mu.Lock()
		if !last.IsZero() && now.Sub(last) < interval {
			mu.Unlock()
			return
		}
		last = now
		mu.Unlock()
		report(issue)
	}
}

// applyListedSSH copies each online machine's listed SSH target into the fields the local
// status and SSH configuration use. The inventory carries it, so a refresh makes no
// per-machine lookups (it used to cost two requests per machine on every refresh).
func applyListedSSH(machines []api.UserMachine) []api.UserMachine {
	for index := range machines {
		machine := &machines[index]
		ssh, ok := machine.CurrentSSH()
		if !machine.Online || machine.State == "revoked" || machine.State == "deleted" || !ok {
			continue
		}
		machine.SSHUser, machine.SSHPort, machine.SSHHostKeys = ssh.OSUser, ssh.Port, ssh.HostKeys
		machine.SSHAuthority = api.SSHAuthority{TargetGeneration: ssh.TargetGeneration, HostKeyGeneration: ssh.HostKeyGeneration}
		if ssh.HostKeyConflict != nil {
			machine.SSHAuthority.HostKeyInUse, machine.SSHAuthority.HostKeyInUseBy = true, ssh.HostKeyConflict.MachineName
		}
	}
	return machines
}

// offlineStreamLifetime caps how long a stream admitted without Paperboat may run; a revocation
// received from the host's signed state still ends it at once.
const offlineStreamLifetime = 12 * time.Hour

// hostRulesPeerStream asks the host to admit the stream from its own signed scopes (F21-11): every SSH stream,
// and exec or terminal streams while Paperboat is unreachable.
func hostRulesPeerStream(request localapi.PeerStreamRequest) (localapi.PeerStreamRequest, error) {
	if !localapi.OfflineConsumers[request.Consumer] {
		return request, errors.New("this session needs Paperboat online")
	}
	request.Offline, request.Credential, request.AccessSessionID = true, "", ""
	request.QUICEndpoint, request.WSSEndpoint = "", ""
	request.Deadline = time.Now().Add(offlineStreamLifetime).UTC()
	if err := request.Validate(time.Now().UTC()); err != nil {
		return request, err
	}
	return request, nil
}

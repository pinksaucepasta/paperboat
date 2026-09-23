package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/spf13/cobra"
)

type desktopRequest struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}

func desktopCommand() *cobra.Command {
	root := &cobra.Command{Use: "desktop", Short: "Authenticated desktop management bridge"}
	request := &cobra.Command{Use: "request", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		data, err := io.ReadAll(io.LimitReader(c.InOrStdin(), 32769))
		if err != nil {
			return writeCLIJSONError(c.OutOrStdout(), err)
		}
		var in desktopRequest
		if len(data) > 32768 {
			err = errors.New("desktop request exceeds 32 KiB")
		} else {
			err = decodeDesktop(data, &in)
		}
		if err != nil {
			return writeCLIJSONError(c.OutOrStdout(), invocationError(err))
		}
		timeout := 45 * time.Second
		if in.Action == "local.update" {
			timeout = 180 * time.Second
		}
		ctx, cancel := context.WithTimeout(c.Context(), timeout)
		defer cancel()
		c.SetContext(ctx)
		out, err := handleDesktop(c, in)
		if err != nil {
			return writeCLIJSONError(c.OutOrStdout(), err)
		}
		return writeCLIJSON(c.OutOrStdout(), out)
	}}
	root.AddCommand(request)
	return root
}

func decodeDesktop(data []byte, out any) error {
	if len(data) == 0 {
		data = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("invalid desktop request: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("desktop request must contain one JSON object")
	}
	return nil
}

func desktopLocalStatus(ctx context.Context) map[string]any {
	out := map[string]any{"version": buildinfo.Version, "platform": runtime.GOOS, "daemon_running": false}
	paths, err := localdaemon.CurrentUserPaths()
	if err == nil {
		var client *localapi.Client
		client, err = localapi.NewClient(paths.SocketPath, time.Second)
		if err == nil {
			probe, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			var snap localapi.Snapshot
			snap, err = client.Snapshot(probe)
			if err == nil {
				out["daemon_running"] = true
				out["daemon_state"] = snap.DaemonState
			}
		}
	}
	if err != nil {
		out["daemon_error"] = "The local Paperboat service is unavailable. Start or repair the service."
	}
	return out
}

func handleDesktop(c *cobra.Command, in desktopRequest) (any, error) {
	// The renderer never chooses a command line, executable, API URL or method.
	switch in.Action {
	case "auth.login":
		return desktopAuthPending(c)
	case "auth.poll":
		return desktopAuthPoll(c)
	case "local.update-status":
		return desktopCLI(c, []string{"update", "status", "--json"})
	case "local.update":
		return desktopCLI(c, []string{"update", "--json"})
	case "local.restart":
		return desktopCLI(c, []string{"daemon", "service", "restart", "--json"})
	case "network.pause":
		return desktopCLI(c, []string{"daemon", "service", "stop", "--json"})
	case "network.resume":
		return desktopCLI(c, []string{"daemon", "service", "start", "--json"})
	case "auth.logout":
		return desktopCLI(c, []string{"auth", "logout", "--json"})
	case "network.get", "network.set", "network.effective", "network.apply":
		return desktopNetwork(c, in)
	}
	client, err := desktopBackend(c)
	if err != nil {
		return nil, err
	}
	ctx := c.Context()
	switch in.Action {
	case "overview":
		me, err := client.Me(ctx)
		if err != nil {
			return nil, err
		}
		devices, err := client.ListUserMachines(ctx)
		if err != nil {
			return nil, err
		}
		teams, err := client.ListTeams(ctx)
		if err != nil {
			return nil, err
		}
		warnings := []string{}
		services, serviceErr := client.DeviceServices(ctx)
		if serviceErr != nil {
			warnings = append(warnings, "Device service details are temporarily unavailable.")
		}
		updates, updateErr := client.MachineUpdateSummary(ctx)
		if updateErr != nil {
			warnings = append(warnings, "Fleet update observations are temporarily unavailable.")
		}
		if devices == nil {
			devices = []api.UserMachine{}
		}
		if teams == nil {
			teams = []api.Team{}
		}
		if services == nil {
			services = []api.DeviceServicesDevice{}
		}
		return map[string]any{"account": me, "devices": devices, "teams": teams, "services": services, "updates": updates, "local": desktopLocalStatus(ctx), "warnings": warnings}, nil
	case "device.rename", "device.disconnect", "device.remove", "device.capabilities", "device.update-status", "device.maintenance", "device.maintenance-list", "device.maintenance-decide":
		var p struct {
			MachineID       string                        `json:"machine_id"`
			Alias           string                        `json:"alias"`
			Description     string                        `json:"description"`
			ExpectedVersion int64                         `json:"expected_version"`
			Desired         api.DeviceCapabilitySelection `json:"desired"`
			Action          string                        `json:"action"`
			TargetVersion   string                        `json:"target_version"`
			Reason          string                        `json:"reason"`
			ApprovalID      string                        `json:"approval_id"`
			Decision        string                        `json:"decision"`
		}
		if err := decodeDesktop(in.Payload, &p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.MachineID) == "" {
			return nil, errors.New("select a device")
		}
		switch in.Action {
		case "device.rename":
			return client.SetMachineMetadata(ctx, p.MachineID, p.Alias, p.Description)
		case "device.disconnect":
			err = client.DisconnectUserMachine(ctx, p.MachineID)
		case "device.remove":
			err = client.DeleteUserMachine(ctx, p.MachineID)
		case "device.capabilities":
			return client.SetUserMachineCapabilities(ctx, p.MachineID, newIdempotencyKey(), p.Desired, p.ExpectedVersion)
		case "device.update-status":
			return client.MachineUpdateStatus(ctx, p.MachineID)
		case "device.maintenance":
			return client.RequestMachineMaintenance(ctx, p.MachineID, newIdempotencyKey(), p.Action, p.TargetVersion, p.Reason)
		case "device.maintenance-list":
			return client.MachineMaintenanceApprovals(ctx, p.MachineID)
		case "device.maintenance-decide":
			if p.ApprovalID == "" {
				return nil, invocationError(errors.New("select a maintenance request"))
			}
			return client.DecideMachineMaintenance(ctx, p.MachineID, p.ApprovalID, p.Decision)
		}
		return map[string]bool{"saved": err == nil}, err
	case "team.get", "team.create", "team.invite", "team.mutate", "team.accept", "team.activity":
		var p struct {
			TeamID             string `json:"team_id"`
			AccountID          string `json:"account_id"`
			ExpectedGeneration uint64 `json:"expected_generation"`
			Action             string `json:"action"`
			Role               string `json:"role"`
			InvitationID       string `json:"invitation_id"`
			Cursor             string `json:"cursor"`
			Confirmation       string `json:"confirmation"`
		}
		if err := decodeDesktop(in.Payload, &p); err != nil {
			return nil, err
		}
		if in.Action != "team.accept" && !validTeamCLIIdentifier(p.TeamID) {
			return nil, errors.New("select a valid team")
		}
		switch in.Action {
		case "team.get":
			return client.GetTeam(ctx, p.TeamID)
		case "team.create":
			return client.CreateTeam(ctx, api.TeamCreateRequest{OperationID: newIdempotencyKey(), TeamID: p.TeamID})
		case "team.invite":
			return client.InviteTeamMember(ctx, p.TeamID, api.TeamInviteRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: p.ExpectedGeneration, AccountID: p.AccountID})
		case "team.mutate":
			return client.MutateTeam(ctx, p.TeamID, api.TeamMutationRequest{OperationID: newIdempotencyKey(), ExpectedGeneration: p.ExpectedGeneration, AccountID: p.AccountID, Action: p.Action, Role: p.Role, Confirmation: p.Confirmation})
		case "team.accept":
			return client.AcceptTeamInvitation(ctx, p.InvitationID, api.TeamAcceptRequest{OperationID: newIdempotencyKey()})
		case "team.activity":
			return client.TeamActivity(ctx, p.TeamID, p.Cursor, 50)
		}
	case "sessions.list":
		var p struct {
			Offset int `json:"offset"`
		}
		if err := decodeDesktop(in.Payload, &p); err != nil {
			return nil, invocationError(err)
		}
		return client.ManagementSessions(ctx, p.Offset)
	case "sessions.revoke":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := decodeDesktop(in.Payload, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "" {
			return nil, errors.New("select a session")
		}
		err = client.RevokeManagementSession(ctx, p.SessionID)
		return map[string]bool{"revoked": err == nil}, err
	}
	return nil, invocationError(errors.New("unknown desktop management action"))
}

func desktopAuthPending(c *cobra.Command) (any, error) {
	cfg, err := config.Load(configPathFlag(c))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
	defer cancel()
	metadata, err := api.New(cfg.ServerURL, config.Credential{}, nil).ClientConfiguration(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieve the Paperboat dashboard enrollment page: %w", err)
	}
	return map[string]any{
		"status":           "pending",
		"verification_uri": metadata.MachinesURL,
		"message":          dashboardEnrollmentGuidance,
	}, nil
}

func desktopAuthPoll(c *cobra.Command) (any, error) {
	ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
	defer cancel()
	c.SetContext(ctx)
	client, err := desktopBackend(c)
	if errors.Is(err, api.ErrUnauthenticated) {
		return desktopAuthPending(c)
	}
	if err != nil {
		return nil, err
	}
	// A stored token is not proof of sign-in: validate it against the account
	// endpoint so expired, revoked, and otherwise rejected credentials remain
	// in the enrollment state.
	if _, err = client.Me(ctx); errors.Is(err, api.ErrUnauthenticated) {
		return desktopAuthPending(c)
	}
	if err != nil {
		return nil, err
	}
	return map[string]string{"status": "signed_in"}, nil
}

func desktopCLI(parent *cobra.Command, args []string) (any, error) {
	child := newRootCommand()
	var out bytes.Buffer
	child.SetOut(&out)
	child.SetErr(io.Discard)
	child.SetIn(strings.NewReader(""))
	child.SetContext(parent.Context())
	if path := configPathFlag(parent); path != "" {
		args = append([]string{"--config", path}, args...)
	}
	child.SetArgs(args)
	if err := child.Execute(); err != nil {
		return nil, err
	}
	var result cliJSONEnvelope
	if json.Unmarshal(out.Bytes(), &result) == nil && result.SchemaVersion != "" {
		if result.Error != nil {
			return nil, errors.New(result.Error.Message)
		}
		return result.Data, nil
	}
	return map[string]bool{"completed": true}, nil
}

func desktopNetwork(c *cobra.Command, in desktopRequest) (any, error) {
	var p struct {
		Scope              string          `json:"scope"`
		TeamID             string          `json:"team_id"`
		ExpectedRevision   json.RawMessage `json:"expected_revision"`
		DeviceSuffix       *string         `json:"device_suffix"`
		DeviceLoopbackCIDR *string         `json:"device_loopback_cidr"`
		SelectedTeamID     *string         `json:"selected_team_id"`
	}
	if err := decodeDesktop(in.Payload, &p); err != nil {
		return nil, err
	}
	cfg, err := config.Load(configPathFlag(c))
	if err != nil {
		return nil, err
	}
	if p.Scope == "local" {
		if in.Action == "network.get" {
			return cfg.LoadNetworkPreferences()
		}
		if in.Action == "network.set" {
			var revision string
			if json.Unmarshal(p.ExpectedRevision, &revision) != nil {
				return nil, errors.New("local expected_revision is required")
			}
			return cfg.SaveNetworkPreferences(config.NetworkPreferences{DeviceSuffix: p.DeviceSuffix, DeviceLoopbackCIDR: p.DeviceLoopbackCIDR}, revision)
		}
	}
	client, err := desktopBackend(c)
	if err != nil {
		return nil, err
	}
	if in.Action == "network.effective" || in.Action == "network.apply" {
		return desktopEffectiveNetwork(c, cfg, client, in.Action == "network.apply")
	}
	if p.Scope != "account" && p.Scope != "team" {
		return nil, errors.New("scope must be local, account, or team")
	}
	team := ""
	if p.Scope == "team" {
		team = p.TeamID
		if !validTeamCLIIdentifier(team) {
			return nil, errors.New("select a team")
		}
	}
	if in.Action == "network.get" {
		return client.NetworkPreferences(c.Context(), team)
	}
	var revision int64
	if json.Unmarshal(p.ExpectedRevision, &revision) != nil || revision < 0 {
		return nil, errors.New("expected_revision is required")
	}
	return client.SetNetworkPreferences(c.Context(), team, api.SetNetworkPreferences{ExpectedRevision: revision, DeviceSuffix: p.DeviceSuffix, DeviceLoopbackCIDR: p.DeviceLoopbackCIDR, SelectedTeamID: p.SelectedTeamID})
}

// Management needs authentication, not construction of terminal transports.
func desktopBackend(c *cobra.Command) (*api.Client, error) {
	cfg, err := config.Load(configPathFlag(c))
	if err != nil {
		return nil, err
	}
	source, err := sessionauth.NewSource(cfg)
	if err != nil {
		return nil, err
	}
	credential, err := source.WithContext(c.Context()).Credential()
	if errors.Is(err, config.ErrNoCredentials) || errors.Is(err, config.ErrSecretNotFound) {
		return nil, api.ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	return api.New(cfg.ServerURL, credential, nil), nil
}

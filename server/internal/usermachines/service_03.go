package usermachines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/access"
	"github.com/pinksaucepasta/paperboat-server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat-server/internal/admission"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
	"net/url"
	"slices"
	"strings"
	"time"
)

func (s *Service) fileTransferDescriptor(ctx context.Context, userID, sourceMachineID, destinationMachineID, cliClientSessionID, sessionID, requestID, manifestDigest string) (FileTransferDescriptor, error) {
	if sourceMachineID == "" || destinationMachineID == "" || cliClientSessionID == "" || s.controlSigner == nil {
		return FileTransferDescriptor{}, ErrNotFound
	}
	if _, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FileTransferDescriptor{}, ErrNotFound
		}
		return FileTransferDescriptor{}, err
	}
	if sessionID != "" {
		owned, err := s.db.Queries().UserOwnsTerminalSession(ctx, dbsqlc.UserOwnsTerminalSessionParams{SessionID: sessionID, UserID: userID})
		if err != nil {
			return FileTransferDescriptor{}, err
		}
		if !owned {
			return FileTransferDescriptor{}, ErrTerminalSessionNotFound
		}
	}
	destination, err := getMachineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: destinationMachineID, UserID: userID, Capability: "files"})
	if errors.Is(err, sql.ErrNoRows) {
		return FileTransferDescriptor{}, ErrNotFound
	}
	if err != nil {
		return FileTransferDescriptor{}, err
	}
	requiresReceive := sessionID == "" || sourceMachineID == destinationMachineID
	if requiresReceive && !slices.Contains(destination.ConfiguredCapabilities, "file_receive") {
		return FileTransferDescriptor{}, ErrMachineCapabilityUnavailable
	}
	if requiresReceive && (!destination.Online || !slices.Contains(destination.ObservedCapabilities, "file_receive")) {
		return FileTransferDescriptor{}, ErrMachineOffline
	}
	if destination.State == "revoked" || destination.State == "disconnected" || destination.State == "deleted" {
		return FileTransferDescriptor{}, ErrNotFound
	}
	if sourceMachineID == destinationMachineID && (destination.State != "online" || destination.SeatState != "occupied") {
		return FileTransferDescriptor{}, ErrMachineOffline
	}
	var acceptedRequest *teaminbox.Request
	if destination.UserID != userID {
		if s.teamInbox == nil || requestID == "" || manifestDigest == "" {
			return FileTransferDescriptor{}, teaminbox.ErrPending
		}
		accepted, acceptErr := s.teamInbox.Consume(ctx, userID, requestID, manifestDigest)
		if acceptErr != nil {
			return FileTransferDescriptor{}, acceptErr
		}
		if accepted.SourceMachineID != sourceMachineID || accepted.DestinationMachineID != destinationMachineID {
			return FileTransferDescriptor{}, teaminbox.ErrConflict
		}
		acceptedRequest = &accepted
	}
	routeMachine := destination
	if sessionID != "" {
		host, hostErr := s.db.Queries().GetUserMachineTerminalSessionHostForUser(ctx, dbsqlc.GetUserMachineTerminalSessionHostForUserParams{SessionID: sessionID, UserID: userID})
		if hostErr == nil {
			routeMachine = host
		} else if !errors.Is(hostErr, sql.ErrNoRows) {
			return FileTransferDescriptor{}, hostErr
		}
	}
	if routeMachine.State == "revoked" || routeMachine.State == "disconnected" || routeMachine.State == "deleted" || sessionID != "" && !slices.Contains(routeMachine.ObservedCapabilities, "terminal_host") {
		return FileTransferDescriptor{}, ErrProvisioningUnavailable
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, routeMachine.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return FileTransferDescriptor{}, ErrProvisioningUnavailable
	}
	if err != nil {
		return FileTransferDescriptor{}, err
	}
	expiresAt := admission.Expiry(s.controlSigner, s.now())
	jti := newID("token")
	accessSessionID := newID("session")
	operationID := sessionID
	if requestID != "" {
		operationID = requestID
	}
	credentialInput := mint.CredentialInput{
		Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: jti,
		IssuedAt: s.now().UTC(), ExpiresAt: expiresAt, CredentialClass: "file_transfer", Scopes: []string{"file:transfer"},
		EnvironmentID: routeMachine.EnvironmentID, MachineID: routeMachine.ID, SourceMachineID: sourceMachineID,
		UserID: userID, CLIClientSessionID: cliClientSessionID, SessionID: sessionID, OperationID: operationID, AssignmentID: accessSessionID,
	}
	if acceptedRequest != nil {
		credentialInput.RequestID, credentialInput.IdempotencyKey, credentialInput.RequestHash = acceptedRequest.RequestID, acceptedRequest.BatchID, acceptedRequest.ManifestDigest
	}
	token, err := admission.Sign(s.controlSigner, credentialInput)
	if err != nil {
		return FileTransferDescriptor{}, err
	}
	httpIngressURL, ingressErr := s.runtimeIngressURL(ctx, routeMachine.EnvironmentID, route.PublicHost)
	if ingressErr != nil {
		return FileTransferDescriptor{}, ingressErr
	}
	httpBaseURL := httpIngressURL
	if err := s.createAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{
		Capabilities: []string{"files"}, OperationID: operationID, ID: accessSessionID, UserMachineID: routeMachine.ID, UserID: userID, EnvironmentID: routeMachine.EnvironmentID,
		CLIClientSessionID: cliClientSessionID, HttpBaseUrl: httpBaseURL, HelperFileSessionID: jti, ExpiresAt: expiresAt,
	}, machineCredentialSigned); err != nil {
		return FileTransferDescriptor{}, err
	}
	return FileTransferDescriptor{
		Endpoint: httpBaseURL + "/v1/file-transfers", SourceMachineID: sourceMachineID,
		DestinationMachineID: destination.ID, InitiatingUserID: userID, Policy: s.fileTransferPolicy,
		Auth: map[string]any{"method": "bearer", "token": token, "expires_at": expiresAt, "scopes": []string{"file:transfer"}, "access_session_id": accessSessionID},
	}, nil
}

func (r ConnectionDescriptor) MarshalJSON() ([]byte, error) {
	if r.Schema != accessdescriptor.SchemaV1 {
		return nil, errors.New("connection descriptor schema is required")
	}
	environment, ok := r.Environment["id"].(string)
	if !ok || environment == "" {
		return nil, errors.New("canonical environment descriptor is incomplete")
	}
	env := accessdescriptor.Environment{
		ID: environment, Kind: stringValue(r.Environment, "kind"), ResourceID: stringValue(r.Environment, "resource_id"),
		Alias: stringValue(r.Environment, "alias"), State: stringValue(r.Environment, "state"), Root: stringValue(r.Environment, "root"),
	}
	if env.Kind == "" || env.ResourceID == "" || env.Alias == "" || env.State == "" {
		return nil, errors.New("canonical environment descriptor is incomplete")
	}
	out := accessdescriptor.Descriptor{Schema: r.Schema, Issuer: r.Issuer, Connectable: r.Connectable, ExpiresAt: r.ExpiresAt, Environment: env, Capabilities: slices.Clone(r.Capabilities), Status: r.Status, Reason: r.Reason, RetryAfterSeconds: r.RetryAfterSeconds}
	if r.Terminal != nil && r.Terminal["auth"] != nil {
		terminal, err := decodeCanonical[accessdescriptor.Terminal](r.Terminal)
		if err != nil {
			return nil, err
		}
		out.Terminal = &terminal
	}
	if r.FileTransfer != nil && r.FileTransfer["auth"] != nil {
		transfer, err := decodeCanonical[accessdescriptor.FileTransfer](r.FileTransfer)
		if err != nil {
			return nil, err
		}
		out.FileTransfer = &transfer
	}
	return json.Marshal(out)
}

func stringValue(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func decodeCanonical[T any](value any) (T, error) {
	var out T
	b, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

func machineConnectionState(connectable bool, state string) string {
	if connectable {
		return "ready"
	}
	switch state {
	case "deleted":
		return "deleted"
	case "revoked", "disconnected":
		return "revoked"
	case "starting", "provisioning":
		return "starting"
	default:
		return "offline"
	}
}

func setCanonicalMachineIdentity(response *ConnectionDescriptor, row dbsqlc.UserMachine) {
	response.MachineGeneration = row.InstallationGeneration
	response.Schema = accessdescriptor.SchemaV1
	response.Capabilities = []string{accessdescriptor.CapabilityTerminal, accessdescriptor.CapabilityFileTransfer, accessdescriptor.CapabilityPreview}
	response.Environment = map[string]any{"id": row.EnvironmentID, "kind": accessdescriptor.EnvironmentMachine, "resource_id": row.ID, "alias": row.Alias, "state": machineConnectionState(response.Connectable, row.State), "root": row.WorkspaceRoot}
}

// ConnectSharedTerminalSession issues only the exact current session role. It
// deliberately bypasses machine capability grants: the session grant is the
// complete authority and confers no file, ENV, exec, or resharing access.
func (s *Service) ConnectSharedTerminalSession(ctx context.Context, userID, sourceMachineID, cliClientSessionID, sessionID string) (ConnectionDescriptor, error) {
	if sourceMachineID == "" || cliClientSessionID == "" || sessionID == "" {
		return ConnectionDescriptor{}, ErrNotFound
	}
	if _, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID}); exactNoRows(err) {
		return ConnectionDescriptor{}, ErrNotFound
	} else if err != nil {
		return ConnectionDescriptor{}, err
	}
	teamFilter := ""
	if scope, selected := workspace.FromContext(ctx); selected {
		if scope.TeamID == "" {
			return ConnectionDescriptor{}, ErrTerminalSessionNotFound
		}
		teamFilter = scope.TeamID
	}
	decision, err := teams.NewService(s.db).ResolveTerminalSessionInTeam(ctx, userID, sessionID, teamFilter)
	if err != nil {
		if err == teams.ErrNotFound {
			return ConnectionDescriptor{}, ErrTerminalSessionNotFound
		}
		return ConnectionDescriptor{}, err
	}
	row, err := s.db.Queries().GetUserMachineForSharedTerminalSession(ctx, dbsqlc.GetUserMachineForSharedTerminalSessionParams{SessionID: sessionID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if !slices.Contains(row.ConfiguredCapabilities, "terminal_host") {
		return ConnectionDescriptor{}, ErrMachineCapabilityUnavailable
	}
	expires := admission.Expiry(s.controlSigner, s.now())
	response := ConnectionDescriptor{Issuer: s.issuer, UserMachineID: row.ID, UserMachineState: row.State, ExpiresAt: expires, Status: "connector_connecting", Reason: "connector_offline", RetryAfterSeconds: 2}
	setCanonicalMachineIdentity(&response, row)
	if row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" {
		response.Status, response.Reason = "user_machine_revoked", "access_revoked"
		return response, nil
	}
	if !row.Online || !slices.Contains(row.ObservedCapabilities, "terminal_host") {
		response.Status, response.Reason = "machine_offline", "terminal_host_unavailable"
		return response, nil
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, row.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return response, nil
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if s.controlSigner == nil {
		return ConnectionDescriptor{}, errors.New("user-machine credential issuer is unavailable")
	}
	now := s.now().UTC()
	snapshotToken, err := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: decision.OwnerAccount, JTI: newID("token"), IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"}, EnvironmentID: row.EnvironmentID, MachineID: row.ID, UserID: decision.OwnerAccount, CLIClientSessionID: "control-plane", SessionID: sessionID})
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	httpIngressURL, ingressErr := s.runtimeIngressURL(ctx, row.EnvironmentID, route.PublicHost)
	if ingressErr != nil {
		return ConnectionDescriptor{}, ingressErr
	}
	snapshot, err := s.controlRuntime.Terminal(ctx, httpIngressURL, snapshotToken, "snapshot", sessionID, newID("operation"))
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if _, err = s.db.SQL().ExecContext(ctx, `UPDATE user_machine_terminal_sessions SET runtime_state=$2,closed_at=CASE WHEN $2 IN ('closed','exited','deleted') THEN coalesce(closed_at,now()) ELSE NULL END,launch_cwd=coalesce(nullif($3,''),launch_cwd),last_runtime_sync_at=now(),last_runtime_sequence=$4,last_activity_at=CASE WHEN $5::timestamptz IS NULL THEN last_activity_at ELSE greatest(coalesce(last_activity_at,$5),$5) END,updated_at=now() WHERE id=$1`, sessionID, snapshot.State, cleanTerminalMetadata(snapshot.CWD, 1024), int64(snapshot.LatestSequence), snapshot.LastActiveAt); err != nil {
		return ConnectionDescriptor{}, err
	}
	if snapshot.State == "closed" || snapshot.State == "exited" || snapshot.State == "deleted" {
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	if snapshot.Generation != decision.ProcessGeneration {
		if fenceErr := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_kind='terminal_session' AND resource_id=$2 AND active AND terminal_process_generation=$3`, decision.TeamID, sessionID, decision.ProcessGeneration)
			return err
		}); fenceErr != nil {
			return ConnectionDescriptor{}, fenceErr
		}
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	var cwd string
	if decision.TargetKind != "machine" {
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	session, e := s.db.Queries().GetSharedUserMachineTerminalSession(ctx, dbsqlc.GetSharedUserMachineTerminalSessionParams{SessionID: sessionID, UserID: userID})
	if exactNoRows(e) {
		return ConnectionDescriptor{}, ErrTerminalSessionNotFound
	}
	if e != nil {
		return ConnectionDescriptor{}, e
	}
	cwd = session.LaunchCwd
	accessID, jti := newID("session"), newID("token")
	scope := "terminal:view"
	if decision.Role == "interactive" {
		scope = "terminal:control"
	}
	token, err := s.signProcessCredential(ctx, mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: userID, JTI: jti, IssuedAt: s.now().UTC(), ExpiresAt: expires, CredentialClass: "terminal_operation", Scopes: []string{scope}, EnvironmentID: row.EnvironmentID, MachineID: row.ID, SourceMachineID: sourceMachineID, UserID: userID, AccountID: userID, CLIClientSessionID: cliClientSessionID, SessionID: sessionID, AssignmentID: accessID, ExpectedGeneration: int64(decision.ProcessGeneration)})
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	params := dbsqlc.CreateUserMachineAccessSessionParams{ID: accessID, UserMachineID: row.ID, UserID: userID, EnvironmentID: row.EnvironmentID, CLIClientSessionID: cliClientSessionID, HttpBaseUrl: httpIngressURL, HelperTerminalSessionID: jti, ExpiresAt: expires, TeamID: sql.NullString{String: decision.TeamID, Valid: true}, Capabilities: []string{}, OperationID: sessionID, TerminalSessionID: sessionID, TerminalRole: decision.Role, TerminalGrantGeneration: sql.NullInt64{Int64: int64(decision.GrantGeneration), Valid: true}, TerminalBindingGeneration: sql.NullInt64{Int64: int64(decision.BindingGeneration), Valid: true}, TerminalMembershipGeneration: sql.NullInt64{Int64: int64(decision.MembershipGeneration), Valid: true}, TerminalTeamGeneration: sql.NullInt64{Int64: int64(decision.TeamGeneration), Valid: true}}
	if err = s.createSharedTerminalAccessSession(ctx, params, sourceMachineID, decision); err != nil {
		cleanupErr := s.revokeCredentialSessions(ctx, machineAccessSession{CredentialKind: machineCredentialSigned, UserID: userID, UserMachineID: row.ID, EnvironmentID: row.EnvironmentID, CLIClientSessionID: cliClientSessionID, HTTPBaseURL: httpIngressURL, TerminalSessionID: jti}, "access_session_persistence_failed")
		return ConnectionDescriptor{}, errors.Join(err, cleanupErr)
	}
	response.Connectable, response.Status, response.Reason, response.RetryAfterSeconds = true, "ready", "ready", 0
	setCanonicalMachineIdentity(&response, row)
	auth := map[string]any{"method": "bearer", "token": token, "expires_at": expires, "scopes": []string{scope}, "access_session_id": accessID}
	response.Terminal = map[string]any{"protocol": "paperboat.terminal.v1", "endpoints": machineTerminalEndpoints(runtimeWebsocketBase(httpIngressURL)), "session_id": sessionID, "cwd": cwd, "role": decision.Role, "auth": auth}
	return response, nil
}

func (s *Service) createSharedTerminalAccessSession(ctx context.Context, in dbsqlc.CreateUserMachineAccessSessionParams, sourceMachineID string, want teams.TerminalSessionDecision) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		var ready bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_machines source JOIN cli_client_sessions cli ON cli.id=$3 AND cli.user_id=$1 AND cli.state='active' AND cli.revoked_at IS NULL JOIN user_machines target ON target.id=$4 WHERE source.id=$2 AND source.user_id=$1 AND source.deleted_at IS NULL AND source.revoked_at IS NULL AND target.state='online' AND target.online AND target.deleted_at IS NULL AND target.revoked_at IS NULL AND target.configured_capabilities @> ARRAY['terminal_host'] AND target.observed_capabilities @> ARRAY['terminal_host'])`, in.UserID, sourceMachineID, in.CLIClientSessionID, in.UserMachineID).Scan(&ready); err != nil {
			return err
		}
		if !ready {
			return ErrTerminalSessionNotFound
		}
		var team, role string
		var tg, mg, bg, gg, pg uint64
		err := tx.QueryRow(ctx, `SELECT d.team_id,d.role,d.team_generation,d.membership_generation,d.binding_generation,d.grant_generation,d.process_generation FROM (SELECT b.team_id,terminal_session_role($1,$2) role,t.generation team_generation,m.membership_generation,b.generation binding_generation,b.terminal_process_generation process_generation,CASE WHEN sg.account_id IS NOT NULL THEN sg.generation ELSE ag.generation END grant_generation FROM team_resource_bindings b JOIN teams t USING(team_id) JOIN team_members m ON m.team_id=b.team_id AND m.account_id=$1 LEFT JOIN team_terminal_session_grants sg ON sg.team_id=b.team_id AND sg.terminal_session_id=$2 AND sg.audience='selected_member' AND sg.account_id=$1 LEFT JOIN team_terminal_session_grants ag ON ag.team_id=b.team_id AND ag.terminal_session_id=$2 AND ag.audience='all_members' WHERE b.resource_kind='terminal_session' AND b.resource_id=$2 AND b.active AND m.active AND t.deleted_at IS NULL) d WHERE d.role IN ('viewer','interactive') ORDER BY d.team_id LIMIT 1`, in.UserID, want.SessionID).Scan(&team, &role, &tg, &mg, &bg, &gg, &pg)
		if exactNoRows(err) {
			return ErrTerminalSessionNotFound
		}
		if err != nil {
			return err
		}
		if team != want.TeamID || role != want.Role || tg != want.TeamGeneration || mg != want.MembershipGeneration || bg != want.BindingGeneration || gg != want.GrantGeneration || pg != want.ProcessGeneration {
			return ErrTerminalSessionNotFound
		}
		in.CredentialKind = string(machineCredentialSigned)
		if err := tx.Queries().CreateUserMachineAccessSession(ctx, in); err != nil {
			return err
		}
		return teams.AuditTx(ctx, tx, in.UserID, want.TeamID, "terminal_session_access_authorized", in.ID, map[string]any{"terminal_session_id": want.SessionID, "role": want.Role, "access_session_id": in.ID})
	})
}

func (s *Service) Connect(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID string) (ConnectionDescriptor, error) {
	return s.ConnectTerminalSession(ctx, userID, sourceMachineID, userMachineID, cliClientSessionID, "")
}

func (s *Service) ConnectTerminalSession(ctx context.Context, userID, sourceMachineID, userMachineID, cliClientSessionID, terminalSessionID string) (ConnectionDescriptor, error) {
	if sourceMachineID == "" {
		return ConnectionDescriptor{}, ErrNotFound
	}
	if _, err := s.db.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: sourceMachineID, UserID: userID}); exactNoRows(err) {
		return ConnectionDescriptor{}, ErrNotFound
	} else if err != nil {
		return ConnectionDescriptor{}, err
	}
	row, err := getMachineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: "terminal"})
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionDescriptor{}, ErrNotFound
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if !slices.Contains(row.ConfiguredCapabilities, "terminal_host") {
		return ConnectionDescriptor{}, ErrMachineCapabilityUnavailable
	}
	terminalSession, err := s.terminalSession(ctx, userID, userMachineID, terminalSessionID)
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	expires := admission.Expiry(s.controlSigner, s.now())
	response := ConnectionDescriptor{Issuer: s.issuer, UserMachineID: row.ID, UserMachineState: row.State, ExpiresAt: expires, Status: "connector_connecting", Reason: "connector_offline", RetryAfterSeconds: 2}
	setCanonicalMachineIdentity(&response, row)
	if row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" {
		response.Status = "user_machine_revoked"
		response.Reason = "access_revoked"
		return response, nil
	}
	if !row.Online || !slices.Contains(row.ObservedCapabilities, "terminal_host") {
		response.Status, response.Reason = "machine_offline", "terminal_host_unavailable"
		return response, nil
	}
	route, routeErr := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, row.EnvironmentID)
	if errors.Is(routeErr, sql.ErrNoRows) {
		return response, nil
	}
	if routeErr != nil {
		return ConnectionDescriptor{}, routeErr
	}
	httpIngressURL, ingressErr := s.runtimeIngressURL(ctx, row.EnvironmentID, route.PublicHost)
	if ingressErr != nil {
		return ConnectionDescriptor{}, ingressErr
	}
	httpBaseURL, websocketBaseURL := httpIngressURL, runtimeWebsocketBase(httpIngressURL)
	if cliClientSessionID == "" || s.controlSigner == nil && s.credentials == nil {
		return ConnectionDescriptor{}, errors.New("user-machine credential issuer is unavailable")
	}
	input := access.CredentialInput{UserID: userID, MachineID: row.ID, EnvironmentID: row.EnvironmentID, SourceMachineID: sourceMachineID, CLIClientSessionID: cliClientSessionID, HTTPBaseURL: httpBaseURL, ExpiresAt: expires}
	if s.controlSigner == nil {
		if err := s.credentials.CheckCLI(ctx, input); err != nil {
			return ConnectionDescriptor{}, err
		}
		if checker, ok := s.credentials.(interface {
			CheckHealth(context.Context, access.CredentialInput) error
		}); ok {
			if err := checker.CheckHealth(ctx, input); err != nil {
				response.Status = "connector_connecting"
				response.Reason = "helper_unhealthy"
				return response, nil
			}
		}
	}
	if err := s.applyTerminalSessionOperationsForSession(ctx, row.ID, terminalSession.ID); err != nil {
		response.Status = "helper_starting"
		response.Reason = "terminal_session_operation_pending"
		return response, nil
	}
	accessSessionID := newID("session")
	input.AccessSessionID = accessSessionID
	personal := row.UserID == userID && !row.OwnerTeamID.Valid
	if scope, selected := workspace.FromContext(ctx); selected {
		personal = personal && scope.TeamID == ""
	}
	includeFiles := personal && slices.Contains(row.ConfiguredCapabilities, "file_receive") && slices.Contains(row.ObservedCapabilities, "file_receive")
	credentials, credentialKind, err := s.issueUserMachineCredentials(ctx, input, terminalSession.ID, includeFiles)
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if len(compactSessionIDs(credentials.TerminalSessionID, credentials.FileSessionID)) == 0 {
		return ConnectionDescriptor{}, errors.New("user-machine credential issuer returned no revocable sessions")
	}
	if credentials.TerminalAuth == nil || includeFiles && credentials.FileTransferAuth == nil {
		return ConnectionDescriptor{}, errors.New("user-machine credential issuer returned incomplete authorization")
	}
	credentials.TerminalAuth["access_session_id"] = accessSessionID
	if credentials.FileTransferAuth != nil {
		credentials.FileTransferAuth["access_session_id"] = accessSessionID
	}
	capabilities := []string{"terminal"}
	if includeFiles {
		capabilities = append(capabilities, "files")
	}
	if personal && slices.Contains(row.ConfiguredCapabilities, "preview_launch") {
		capabilities = append(capabilities, "private_access")
	}
	if err := s.createAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{
		Capabilities: capabilities, OperationID: terminalSession.ID, ID: accessSessionID, UserMachineID: row.ID, UserID: userID, EnvironmentID: row.EnvironmentID,
		CLIClientSessionID: cliClientSessionID, HttpBaseUrl: httpBaseURL,
		HelperTerminalSessionID: credentials.TerminalSessionID, HelperFileSessionID: credentials.FileSessionID,
		ExpiresAt: expires,
	}, credentialKind); err != nil {
		cleanupErr := s.revokeCredentialSessions(ctx, machineAccessSession{
			UserID: userID, UserMachineID: row.ID, EnvironmentID: row.EnvironmentID,
			CLIClientSessionID: cliClientSessionID, HTTPBaseURL: httpBaseURL,
			TerminalSessionID: credentials.TerminalSessionID, FileSessionID: credentials.FileSessionID, CredentialKind: credentialKind,
		}, "access_session_persistence_failed")
		return ConnectionDescriptor{}, errors.Join(err, cleanupErr)
	}
	response.Connectable, response.Status, response.Reason, response.RetryAfterSeconds = true, "ready", "ready", 0
	setCanonicalMachineIdentity(&response, row)
	response.Terminal = map[string]any{"protocol": "paperboat.terminal.v1", "endpoints": machineTerminalEndpoints(websocketBaseURL), "session_id": terminalSession.ID, "cwd": terminalSession.LaunchCwd, "auth": credentials.TerminalAuth}
	if credentials.FileTransferAuth != nil {
		response.FileTransfer = map[string]any{"endpoint": httpBaseURL + "/v1/file-transfers", "source_machine_id": sourceMachineID, "destination_machine_id": row.ID, "initiating_user_id": userID, "policy": s.fileTransferPolicy, "auth": credentials.FileTransferAuth}
	}
	return response, nil
}

func machineTerminalEndpoints(wss string) map[string]any {
	u, err := url.Parse(wss)
	if err != nil || u.Hostname() == "" {
		return map[string]any{}
	}
	host := u.Host
	if u.Port() == "" {
		host += ":443"
	}
	endpoint := "wss://" + u.Host + strings.TrimRight(u.Path, "/") + "/v1/runtime"
	if u.Path != "" {
		return map[string]any{"wss": endpoint}
	}
	return map[string]any{"quic": "quic://" + host, "wss": endpoint}
}

func (s *Service) issueUserMachineCredentials(ctx context.Context, input access.CredentialInput, terminalSessionID string, includeFiles bool) (access.CLICredentials, machineCredentialKind, error) {
	if s.controlSigner == nil {
		credentials, err := s.credentials.IssueCLI(ctx, input)
		return credentials, machineCredentialExternal, err
	}
	issuedAt := s.now().UTC()
	terminalJTI := newID("token")
	transferJTI := newID("token")
	sign := func(class string, scopes []string, jti string) (string, error) {
		return s.signProcessCredential(ctx, mint.CredentialInput{
			Issuer: s.issuer, Audience: "paperboat-machine", Subject: input.UserID, JTI: jti,
			IssuedAt: issuedAt, ExpiresAt: input.ExpiresAt, CredentialClass: class, Scopes: scopes,
			EnvironmentID: input.EnvironmentID, MachineID: input.MachineID, SourceMachineID: input.SourceMachineID, UserID: input.UserID, CLIClientSessionID: input.CLIClientSessionID, SessionID: terminalSessionID, AssignmentID: input.AccessSessionID,
		})
	}
	terminalToken, err := sign("terminal_operation", []string{"terminal:operate"}, terminalJTI)
	if err != nil {
		return access.CLICredentials{}, machineCredentialSigned, err
	}
	if !includeFiles {
		return access.CLICredentials{TerminalAuth: map[string]any{"method": "bearer", "token": terminalToken, "expires_at": input.ExpiresAt, "scopes": []string{"terminal:operate"}}, TerminalSessionID: terminalJTI}, machineCredentialSigned, nil
	}
	transferToken, err := sign("file_transfer", []string{"file:transfer"}, transferJTI)
	if err != nil {
		return access.CLICredentials{}, machineCredentialSigned, err
	}
	return access.CLICredentials{
		TerminalAuth:      map[string]any{"method": "bearer", "token": terminalToken, "expires_at": input.ExpiresAt, "scopes": []string{"terminal:operate"}},
		FileTransferAuth:  map[string]any{"method": "bearer", "token": transferToken, "expires_at": input.ExpiresAt, "scopes": []string{"file:transfer"}},
		TerminalSessionID: terminalJTI, FileSessionID: transferJTI,
	}, machineCredentialSigned, nil
}

func (s *Service) ConnectionReadiness(ctx context.Context, userID, userMachineID string) (ConnectionDescriptor, error) {
	return s.ConnectionReadinessForTerminalSession(ctx, userID, userMachineID, "")
}

func (s *Service) ConnectionReadinessForTerminalSession(ctx context.Context, userID, userMachineID, terminalSessionID string) (ConnectionDescriptor, error) {
	row, err := getMachineForCapability(ctx, s.db.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: userMachineID, UserID: userID, Capability: "terminal"})
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionDescriptor{}, ErrNotFound
	}
	if err != nil {
		return ConnectionDescriptor{}, err
	}
	if _, err := s.terminalSession(ctx, userID, userMachineID, terminalSessionID); err != nil {
		return ConnectionDescriptor{}, err
	}
	response := ConnectionDescriptor{Issuer: s.issuer, UserMachineID: row.ID, UserMachineState: row.State, ExpiresAt: s.now().UTC(), Status: "connector_connecting", Reason: "connector_offline", RetryAfterSeconds: 2}
	setCanonicalMachineIdentity(&response, row)
	if row.State == "revoked" || row.State == "disconnected" || row.State == "deleted" || row.SeatState != "occupied" {
		response.Status = "user_machine_revoked"
		response.Reason = "access_revoked"
		return response, nil
	}
	if _, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, row.EnvironmentID); errors.Is(err, sql.ErrNoRows) {
		return response, nil
	} else if err != nil {
		return ConnectionDescriptor{}, err
	}
	response.Connectable, response.Status, response.Reason, response.RetryAfterSeconds = true, "ready", "ready", 0
	setCanonicalMachineIdentity(&response, row)
	return response, nil
}

func (s *Service) terminalSession(ctx context.Context, userID, userMachineID, sessionID string) (dbsqlc.UserMachineTerminalSession, error) {
	if strings.TrimSpace(sessionID) == "" {
		return dbsqlc.UserMachineTerminalSession{}, ErrTerminalSessionNotFound
	}
	row, err := getTerminalSession(ctx, s.db.Queries(), dbsqlc.GetUserMachineTerminalSessionParams{ID: sessionID, UserMachineID: userMachineID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.UserMachineTerminalSession{}, ErrTerminalSessionNotFound
	}
	return row, err
}

func (s *Service) Approve(ctx context.Context, userID, userCode string) (UserMachine, error) {
	return s.approve(ctx, userID, userCode)
}

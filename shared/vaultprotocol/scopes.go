package vaultprotocol

type VaultScopeState struct {
	WorkspaceID  string `json:"workspace_id"`
	WriterPublic string `json:"writer_public"`
	OwnerKind    string `json:"owner_kind"`
	OwnerID      string `json:"owner_id"`
	MachineID    string `json:"machine_id"`
	KeyEpoch     uint64 `json:"key_epoch"`
	Revision     uint64 `json:"revision"`
	DocumentID   string `json:"document_id"`
	Envelope     string `json:"envelope"`
}
type VaultScopePut struct {
	OperationID string `json:"operation_id"`
	Envelope    string `json:"envelope"`
}
type VaultTeamCreate struct {
	ExpectedTeamGeneration uint64 `json:"expected_team_generation"`
	VaultEnvelope          string `json:"vault_envelope"`
	OperationID            string `json:"operation_id"`
	TeamID                 string `json:"team_id"`
	ScopeEnvelope          string `json:"scope_envelope"`
	GrantEnvelope          string `json:"grant_envelope"`
}
type VaultTeamMember struct {
	EnvPermission        string `json:"env_permission"`
	AccountID            string `json:"account_id"`
	MembershipGeneration uint64 `json:"membership_generation"`
	Role                 string `json:"role"`
	Active               bool   `json:"active"`
	GrantEpoch           uint64 `json:"grant_epoch"`
}
type VaultTeamState struct {
	RecordSequence uint64            `json:"record_sequence"`
	Entitled       bool              `json:"entitled"`
	TeamID         string            `json:"team_id"`
	OwnerAccount   string            `json:"owner_account"`
	Generation     uint64            `json:"generation"`
	KeyEpoch       uint64            `json:"key_epoch"`
	Members        []VaultTeamMember `json:"members"`
	Scope          VaultScopeState   `json:"scope"`
}
type VaultMemberGrant struct {
	OperationID                  string `json:"operation_id"`
	AccountID                    string `json:"account_id"`
	ExpectedTeamGeneration       uint64 `json:"expected_team_generation"`
	ExpectedMembershipGeneration uint64 `json:"expected_membership_generation"`
	GrantEnvelope                string `json:"grant_envelope"`
}
type VaultTeamRotate struct {
	Records                VaultRecordReplacement `json:"records"`
	ConfirmTotalLoss       bool                   `json:"confirm_total_loss"`
	VaultEnvelope          string                 `json:"vault_envelope"`
	OperationID            string                 `json:"operation_id"`
	ExpectedTeamGeneration uint64                 `json:"expected_team_generation"`
	RemoveAccountIDs       []string               `json:"remove_account_ids"`
	ScopeEnvelope          string                 `json:"scope_envelope"`
	GrantEnvelopes         []string               `json:"grant_envelopes"`
}
type VaultGrantState struct {
	Acknowledged         bool   `json:"acknowledged"`
	SenderWriterPublic   string `json:"sender_writer_public"`
	TeamID               string `json:"team_id"`
	MembershipGeneration uint64 `json:"membership_generation"`
	TeamEpoch            uint64 `json:"team_epoch"`
	DocumentID           string `json:"document_id"`
	Envelope             string `json:"envelope"`
}
type VaultSharingState struct {
	AccountID       string `json:"account_id"`
	VaultGeneration uint64 `json:"vault_generation"`
	WriterPublic    string `json:"writer_public"`
	SharingPublic   string `json:"sharing_public"`
}
type VaultPersonalReset struct {
	OperationID             string   `json:"operation_id"`
	ExpectedVaultDocumentID string   `json:"expected_vault_document_id"`
	VaultEnvelope           string   `json:"vault_envelope"`
	ScopeEnvelopes          []string `json:"scope_envelopes"`
	ConfirmTotalLoss        bool     `json:"confirm_total_loss"`
}

type VaultPersonalScopes struct {
	KeyEpoch uint64            `json:"key_epoch"`
	Scopes   []VaultScopeState `json:"scopes"`
}

type VaultPersonalRotate struct {
	OperationID             string               `json:"operation_id"`
	ExpectedVaultDocumentID string               `json:"expected_vault_document_id"`
	VaultEnvelope           string               `json:"vault_envelope"`
	ScopeDocuments          []VaultScopeDocument `json:"scope_documents"`
}
type VaultScopeDocument struct {
	WorkspaceID string `json:"workspace_id"`
	MachineID   string `json:"machine_id"`
	DocumentID  string `json:"document_id"`
}
type VaultPersonalScopeStage struct {
	Records                 VaultRecordReplacement `json:"records"`
	WorkspaceID             string                 `json:"workspace_id"`
	ExpectedVaultDocumentID string                 `json:"expected_vault_document_id"`
	MachineID               string                 `json:"machine_id"`
	Envelope                string                 `json:"envelope"`
}

type VaultRecordReplacement struct {
	ExpectedSequence uint64   `json:"expected_sequence"`
	DocumentIDs      []string `json:"document_ids"`
	Envelopes        []string `json:"envelopes"`
}

type PasswordVaultHead struct {
	Issuer     string `json:"issuer"`
	AccountID  string `json:"account_id"`
	Generation uint64 `json:"generation"`
	DocumentID string `json:"document_id"`
	Envelope   string `json:"envelope"`
}

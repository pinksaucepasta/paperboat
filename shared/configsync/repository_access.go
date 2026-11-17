// Package configsync is the config-sync wire between the control plane and the machine that syncs a config
// repository. The access document carries a scoped credential: it is stored encrypted on the server and decoded
// strictly on replay, so a new field is a new stored shape on both sides at once.
package configsync

import "time"

// RepositoryAccess is the short-lived, scoped access a machine receives for one assignment's repository.
type RepositoryAccess struct {
	RepositoryID  string    `json:"repository_id"`
	AssignmentID  string    `json:"assignment_id"`
	EnvironmentID string    `json:"environment_id"`
	MachineID     string    `json:"machine_id"`
	CloneURL      string    `json:"clone_url"`
	PublishURL    string    `json:"publish_url"`
	Branch        string    `json:"branch"`
	Transport     string    `json:"transport"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	Capability    string    `json:"capability"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// Package admission defines the verified credential claims auth.Verifier.VerifyHelperAccess returns,
// and the connector's current authority the control client reads.
package admission

import "time"

type Claims struct {
	KeyID                  string
	Issuer                 string
	Audience               string
	JTI                    string
	CredentialClass        string
	Scopes                 []string
	EnvironmentID          string
	MachineID              string
	InstallationGeneration int64
	HelperID               string
	ConnectorID            string
	ConnectorGeneration    uint64
	EdgePool               string
	EdgeNodeID             string
	RouteBinding           string
	// AssignmentID is a runtime credential's access session: the server's usage
	// accounting authority for the session's bytes through the edge.
	AssignmentID string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	Revoked      bool
}

type Current struct {
	Generation uint64
	EdgePool   string
	EdgeNode   string
	Revoked    bool
}

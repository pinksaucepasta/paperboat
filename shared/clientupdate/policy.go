// Package clientupdate is the public client update policy document: the server serves it (GET /v1/client/update-policy)
// and every pb binary reads it. How a version compares to the floor stays with each side (the server gates with its
// calendar parser, the cli judges for its self-update prompt).
package clientupdate

import "time"

const Schema = "paperboat.client-update-policy/v1"

// Policy is the minimum client version, why, and when it is enforced.
type Policy struct {
	Schema         string     `json:"schema"`
	Revision       int64      `json:"revision"`
	MinimumVersion string     `json:"minimum_version"`
	Reason         string     `json:"reason"`
	EnforceAt      *time.Time `json:"enforce_at,omitempty"`
}

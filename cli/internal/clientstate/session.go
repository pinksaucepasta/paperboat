// Package clientstate defines per-instance credentials independently of profile paths or keyrings.
package clientstate

import "time"

// Session contains renewable session state, never the API token used to start it.
type Session struct {
	Issuer         string    `json:"issuer"`
	ID             string    `json:"session_id"`
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	TokenType      string    `json:"token_type"`
	ExpiresAt      time.Time `json:"expires_at"`
	RefreshAttempt string    `json:"refresh_attempt,omitempty"`
	Revoking       bool      `json:"revoking,omitempty"`
}

// Storage commits a complete state atomically. Its owner holds exclusive custody for its lifetime.
type Storage interface {
	Load() (Session, error)
	Save(Session) error
	Clear() error
}

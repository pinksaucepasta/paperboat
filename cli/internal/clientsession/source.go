// Package clientsession owns serialized renewable credentials for an embedded client.
package clientsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
	clientstate "github.com/pinksaucepasta/paperboat/shared/client/state"
)

const RefreshBefore = time.Minute

var ErrSignedOut = errors.New("Paperboat session is signed out; start a new client with an API token")

// Source owns refresh transactions; storage and the clock belong to its caller.
type Source struct {
	mu    sync.Mutex
	API   *clientapi.Client
	Store clientstate.Storage
	Now   func() time.Time
}

func (s *Source) Login(ctx context.Context, token, label, osName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.Store.Load()
	if err != nil {
		return err
	}
	if current.ID != "" {
		return errors.New("Paperboat session already exists")
	}
	tokens, err := s.API.StartAPITokenSession(ctx, token, label, "server", osName)
	if err != nil {
		return err
	}
	next, err := s.fromTokens(tokens)
	if err == nil {
		err = s.Store.Save(next)
	}
	if err != nil {
		if tokens.RefreshToken == "" {
			return err
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(err, s.API.RevokeToken(cleanupCtx, tokens.RefreshToken))
	}
	return nil
}

func (s *Source) fromTokens(tokens clientapi.TokenSet) (clientstate.Session, error) {
	if strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.RefreshToken) == "" || tokens.CLIClientSessionID == "" || !strings.EqualFold(tokens.TokenType, "Bearer") || tokens.ExpiresIn <= 0 || int64(tokens.ExpiresIn) > math.MaxInt64/int64(time.Second) {
		return clientstate.Session{}, errors.New("paperboat-server returned an invalid session")
	}
	return clientstate.Session{Issuer: s.API.BaseURL, ID: tokens.CLIClientSessionID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, TokenType: tokens.TokenType, ExpiresAt: s.Now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second)}, nil
}

// Credential saves an attempt before consuming its bearer, then atomically commits the successor.
func (s *Source) Credential(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range 2 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		current, err := s.Store.Load()
		if err != nil {
			return "", err
		}
		if current.ID == "" || current.Revoking {
			return "", ErrSignedOut
		}
		if current.Issuer != s.API.BaseURL {
			return "", errors.New("SDK state belongs to another server")
		}
		if s.Now().Add(RefreshBefore).Before(current.ExpiresAt) && current.RefreshAttempt == "" {
			return current.AccessToken, nil
		}
		if current.RefreshAttempt == "" {
			var nonce [32]byte
			if _, err = rand.Read(nonce[:]); err != nil {
				return "", err
			}
			current.RefreshAttempt = hex.EncodeToString(nonce[:])
			if err = s.Store.Save(current); err != nil {
				return "", err
			}
		}
		tokens, err := s.API.RefreshToken(ctx, current.RefreshToken, current.RefreshAttempt)
		if err != nil {
			return "", err
		}
		next, err := s.fromTokens(tokens)
		if err != nil {
			return "", err
		}
		if next.ID != current.ID {
			return "", errors.New("paperboat-server changed the session during refresh")
		}
		if err = s.Store.Save(next); err != nil {
			return "", err
		}
		if s.Now().Before(next.ExpiresAt) {
			return next.AccessToken, nil
		}
	}
	return "", ErrSignedOut
}

// Logout retains a failed revocation for retry and prevents further authenticated work.
func (s *Source) Logout(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.Store.Load()
	if err != nil {
		return err
	}
	if current.ID == "" {
		return nil
	}
	if !current.Revoking {
		current.Revoking = true
		if err = s.Store.Save(current); err != nil {
			return err
		}
	}
	if err = s.API.RevokeToken(ctx, current.RefreshToken); err != nil {
		return err
	}
	return s.Store.Clear()
}

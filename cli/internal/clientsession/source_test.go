package clientsession

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/client/api"
	clientstate "github.com/pinksaucepasta/paperboat/shared/client/state"
)

type sessionStore struct {
	value      clientstate.Session
	failCommit bool
}

func (s *sessionStore) Load() (clientstate.Session, error) { return s.value, nil }
func (s *sessionStore) Save(v clientstate.Session) error {
	if s.failCommit && v.AccessToken == "successor" {
		return errors.New("state commit failed")
	}
	s.value = v
	return nil
}
func (s *sessionStore) Clear() error { s.value = clientstate.Session{}; return nil }

func TestRefreshRetainsAttemptAcrossFailedCommit(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	attempts := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token/refresh" || r.Header.Get("Authorization") != "Bearer original-refresh" {
			t.Error("refresh identity changed")
		}
		var body struct {
			AttemptID string `json:"attempt_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		attempts = append(attempts, body.AttemptID)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": api.TokenSet{AccessToken: "successor", RefreshToken: "successor-refresh", TokenType: "Bearer", ExpiresIn: 3600, CLIClientSessionID: "session-test"}})
	}))
	defer server.Close()
	store := &sessionStore{value: clientstate.Session{Issuer: server.URL, ID: "session-test", AccessToken: "original", RefreshToken: "original-refresh", ExpiresAt: now.Add(-time.Minute)}, failCommit: true}
	source := &Source{API: &api.Client{BaseURL: server.URL, Options: api.CallOptions{HTTPClient: server.Client()}}, Store: store, Now: func() time.Time { return now }}
	if _, err := source.Credential(context.Background()); err == nil {
		t.Fatal("failed commit accepted")
	}
	if store.value.RefreshToken != "original-refresh" || len(store.value.RefreshAttempt) != 64 {
		t.Fatal("original refresh custody lost")
	}
	store.failCommit = false
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			token, err := source.Credential(context.Background())
			if err != nil || token != "successor" {
				t.Errorf("token refresh: %v", err)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0] != attempts[1] {
		t.Fatalf("attempts were not replayed exactly: calls=%d", len(attempts))
	}
	if store.value.RefreshAttempt != "" || store.value.RefreshToken != "successor-refresh" {
		t.Fatal("successor not committed")
	}
}

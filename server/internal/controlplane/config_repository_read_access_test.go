package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConfigRepositoryReadAccessOwnerIsolationRevocationAndReadOnly(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	owner, other := "bootstrap_owner_"+suffix, "bootstrap_other_"+suffix
	for _, user := range []string{owner, other} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$1,$2,'active')`, user, user+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1 OR id=$2`, owner, other)
	})
	assignments := NewConfigAssignmentService(store, nil, "warning-current")
	assignments.SetRepositoryResolver(ConfigRepositoryResolverFunc(func(ctx context.Context, user, provider, id string) (ConfigRepositoryConnection, error) {
		connection, err := testConfigRepositoryResolver().ResolveConfigRepository(ctx, user, provider, id)
		connection.CredentialCapability = "provider_user_repository"
		return connection, err
	}))
	custom, err := assignments.ConnectRepository(ctx, owner, "git", "/srv/private-config.git", "Custom")
	if err != nil {
		t.Fatal(err)
	}
	reconnected, err := assignments.ConnectRepository(ctx, owner, "git", "/srv/private-config.git", "Custom")
	if err != nil || reconnected.ID != custom.ID {
		t.Fatalf("custom repository identity changed on reconnect: %q, %v", reconnected.ID, err)
	}
	github, err := assignments.ConnectRepository(ctx, owner, "github", "123", "GitHub")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	calls := 0
	revoke := false
	issuer := ConfigRepositoryAccessIssuerFuncs{Issue: func(ctx context.Context, user, id, permission string) (ScopedRepositoryCredential, error) {
		calls++
		if user != owner || id != github.ExternalRepositoryID.String || permission != "read" {
			t.Fatal("provider grant is not exact owner/repository/read permission")
		}
		if revoke {
			if err := assignments.DisconnectRepository(ctx, owner, github.ID); err != nil {
				t.Fatal(err)
			}
		}
		return ScopedRepositoryCredential{Token: "private-fixture-token", ExpiresAt: now.Add(time.Hour)}, nil
	}}
	service := NewConfigRepositoryAccessService(store, nil, issuer, "")
	service.clock = func() time.Time { return now }
	for _, id := range []string{custom.ID, github.ID} {
		if _, err := service.IssueReadAccess(ctx, other, id, ""); !errors.Is(err, ErrConfigRepositoryAccessInvalid) {
			t.Fatal("other account gained read access")
		}
	}
	if calls != 0 {
		t.Fatal("provider contacted before ownership authorization")
	}
	access, err := service.IssueReadAccess(ctx, owner, custom.ID, "ssh://git@lan/private-config.git")
	if err != nil || access.RepositoryID != custom.ID || access.Transport != "ssh" || access.Username != "" || access.Password != "" || access.Capability != "repository_contents_read" || !access.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatal("custom bootstrap authority incorrect")
	}
	if calls != 0 {
		t.Fatal("custom repository requested provider secrets")
	}
	if _, err := service.IssueReadAccess(ctx, owner, custom.ID, "https://token@host/config.git"); err == nil {
		t.Fatal("embedded credential alias accepted")
	}
	if _, err := service.IssueReadAccess(ctx, owner, github.ID, "https://mirror/config.git"); err == nil {
		t.Fatal("provider token redirected")
	}
	access, err = service.IssueReadAccess(ctx, owner, github.ID, "")
	if err != nil || access.Transport != "https" || access.Username != "x-access-token" || access.Password == "" || access.Capability != "repository_contents_read" || !access.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatal("provider bootstrap read grant incorrect")
	}
	providerFailure := errors.New("provider access request failed")
	failingProvider := NewConfigRepositoryAccessService(store, nil, ConfigRepositoryAccessIssuerFuncs{Issue: func(context.Context, string, string, string) (ScopedRepositoryCredential, error) {
		return ScopedRepositoryCredential{}, providerFailure
	}}, "")
	if _, err := failingProvider.IssueReadAccess(ctx, owner, github.ID, ""); !errors.Is(err, ErrConfigRepositoryAccessUncertain) || !errors.Is(err, providerFailure) {
		t.Fatalf("provider issuance cause = %v", err)
	}
	revoke = true
	if _, err := service.IssueReadAccess(ctx, owner, github.ID, ""); !errors.Is(err, ErrConfigRepositoryAccessInvalid) {
		t.Fatal("revoked during issuance returned credentials")
	}
	if err := assignments.DisconnectRepository(ctx, owner, custom.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.IssueReadAccess(ctx, owner, custom.ID, ""); !errors.Is(err, ErrConfigRepositoryAccessInvalid) {
		t.Fatal("disconnected repository read access accepted")
	}
	github, err = assignments.ConnectRepository(ctx, owner, "github", "123", "GitHub")
	if err != nil {
		t.Fatal(err)
	}
	closingProvider := NewConfigRepositoryAccessService(store, nil, ConfigRepositoryAccessIssuerFuncs{Issue: func(context.Context, string, string, string) (ScopedRepositoryCredential, error) {
		if err := store.Close(); err != nil {
			return ScopedRepositoryCredential{}, err
		}
		return ScopedRepositoryCredential{Token: "private-fixture-token", ExpiresAt: now.Add(time.Hour)}, nil
	}}, "")
	if _, err := closingProvider.IssueReadAccess(ctx, owner, github.ID, ""); err == nil || errors.Is(err, ErrConfigRepositoryAccessInvalid) {
		t.Fatalf("post-provider database failure was collapsed into invalid authorization: %v", err)
	}
}

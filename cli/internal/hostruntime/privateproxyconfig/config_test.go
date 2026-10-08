package privateproxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type fakeAdapter struct {
	mu                 sync.Mutex
	state              string
	installs, restores int
	installErr         error
	installState       string
	restoreErr         error
}

func (f *fakeAdapter) Name() string { return "fake" }
func (f *fakeAdapter) Snapshot(context.Context) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return json.Marshal(f.state)
}
func (f *fakeAdapter) Install(_ context.Context, pac string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = pac
	if f.installState != "" {
		f.state = f.installState
	}
	f.installs++
	return f.installErr
}
func (f *fakeAdapter) Owns(_ context.Context, pac string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state == pac, nil
}
func (f *fakeAdapter) Matches(_ context.Context, raw json.RawMessage) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var want string
	if err := json.Unmarshal(raw, &want); err != nil {
		return false, err
	}
	return f.state == want, nil
}
func (f *fakeAdapter) Restore(_ context.Context, raw json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restoreErr != nil {
		return f.restoreErr
	}
	if err := json.Unmarshal(raw, &f.state); err != nil {
		return err
	}
	f.restores++
	return nil
}

func TestManagerInstallRemoveExactState(t *testing.T) {
	f := &fakeAdapter{state: "http://previous.example/proxy.pac"}
	path := filepath.Join(t.TempDir(), "proxy.json")
	m, _ := New(path, f)
	pac := "http://127.0.0.1:37491/private/token.pac"
	if err := m.Install(context.Background(), pac); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode: %v %v", info, err)
	}
	if err := m.Install(context.Background(), pac); err != nil {
		t.Fatal(err)
	}
	if f.installs != 1 {
		t.Fatalf("installs=%d", f.installs)
	}
	if err := m.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.state != "http://previous.example/proxy.pac" {
		t.Fatalf("state=%q", f.state)
	}
	if err := m.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRejectsUntrustedAndExternalChange(t *testing.T) {
	f := &fakeAdapter{state: "off"}
	m, _ := New(filepath.Join(t.TempDir(), "j"), f)
	for _, pac := range []string{"http://proxy.example/p.pac", "https://127.0.0.1:1/p.pac", "http://localhost:1/p.pac", "http://127.0.0.1:1/"} {
		if !errors.Is(m.Install(context.Background(), pac), ErrUntrustedPAC) {
			t.Fatalf("accepted %q", pac)
		}
	}
	if err := m.Install(context.Background(), "http://[::1]:99/a.pac"); err != nil {
		t.Fatal(err)
	}
	f.state = "user-change"
	if !errors.Is(m.Remove(context.Background()), ErrConflict) {
		t.Fatal("expected conflict")
	}
	if f.state != "user-change" {
		t.Fatal("overwrote external change")
	}
}

func TestManagerRecoverPreparedJournal(t *testing.T) {
	f := &fakeAdapter{state: "http://127.0.0.1:9/a.pac"}
	path := filepath.Join(t.TempDir(), "j")
	m, _ := New(path, f)
	prior, _ := json.Marshal("exact-prior")
	if err := m.write(journal{Version: 1, Adapter: "fake", PACURL: "http://127.0.0.1:9/a.pac", Phase: "prepared", Prior: prior}); err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.state != "exact-prior" {
		t.Fatalf("state=%q", f.state)
	}
}

func TestManagerRecoverRemovesAppliedJournalAfterPriorStateWasRestored(t *testing.T) {
	f := &fakeAdapter{state: "exact-prior"}
	path := filepath.Join(t.TempDir(), "j")
	m, _ := New(path, f)
	prior, _ := json.Marshal("exact-prior")
	if err := m.write(journal{Version: 1, Adapter: "fake", PACURL: "http://127.0.0.1:9/a.pac", Phase: "applied", Prior: prior}); err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale journal remains: %v", err)
	}
}

func TestManagerConcurrentInstallIsIdempotent(t *testing.T) {
	f := &fakeAdapter{state: "off"}
	m, _ := New(filepath.Join(t.TempDir(), "j"), f)
	pac := "http://127.0.0.1:1/a.pac"
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- m.Install(context.Background(), pac) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.installs != 1 {
		t.Fatalf("installs=%d", f.installs)
	}
}

func (f *fakeAdapter) OwnsTransition(ctx context.Context, oldURL, newURL string) (bool, error) {
	owned, err := f.Owns(ctx, oldURL)
	if err != nil || owned {
		return owned, err
	}
	return f.Owns(ctx, newURL)
}

func TestManagerRefreshPreservesOriginalStateAndAvoidsChurn(t *testing.T) {
	ctx := context.Background()
	f := &fakeAdapter{state: "original-user-proxy"}
	m, _ := New(filepath.Join(t.TempDir(), "j"), f)
	first, second := "http://127.0.0.1:99/proxy-first.pac", "http://127.0.0.1:99/proxy-second.pac"
	if err := m.Install(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(ctx, first); err != nil {
		t.Fatal(err)
	}
	if f.installs != 1 {
		t.Fatal("unchanged PAC rewrote system settings")
	}
	if err := m.Refresh(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(ctx, "http://127.0.0.1:100/proxy-third.pac"); !errors.Is(err, ErrUntrustedPAC) {
		t.Fatalf("origin change: %v", err)
	}
	if err := m.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if f.state != "original-user-proxy" {
		t.Fatalf("state=%q", f.state)
	}
}

func TestManagerRefreshFailureRollbackAndRetry(t *testing.T) {
	ctx := context.Background()
	f := &fakeAdapter{state: "original"}
	m, _ := New(filepath.Join(t.TempDir(), "j"), f)
	first, second := "http://127.0.0.1:99/first.pac", "http://127.0.0.1:99/second.pac"
	if err := m.Install(ctx, first); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("notification failed after write")
	f.installErr = failure
	if err := m.Refresh(ctx, second); !errors.Is(err, failure) {
		t.Fatalf("refresh: %v", err)
	}
	if f.state != first {
		t.Fatalf("failed refresh lost working PAC: %q", f.state)
	}
	f.installErr = nil
	if err := m.Refresh(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if f.state != "original" {
		t.Fatalf("state=%q", f.state)
	}
}

func TestManagerInterruptedRefreshRecoveryAndRetry(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "retry"}[retry], func(t *testing.T) {
			ctx := context.Background()
			f := &fakeAdapter{state: "original"}
			path := filepath.Join(t.TempDir(), "j")
			m, _ := New(path, f)
			first, second := "http://127.0.0.1:99/first.pac", "http://127.0.0.1:99/second.pac"
			if err := m.Install(ctx, first); err != nil {
				t.Fatal(err)
			}
			f.installErr = errors.New("partial apply")
			f.restoreErr = errors.New("rollback unavailable")
			if err := m.Refresh(ctx, second); err == nil {
				t.Fatal("expected failed refresh")
			}
			j, err := m.read()
			if err != nil || j.Phase != "refreshing" {
				t.Fatalf("journal=%+v err=%v", j, err)
			}
			restarted, _ := New(path, f)
			f.installErr, f.restoreErr = nil, nil
			if retry {
				if err := restarted.Refresh(ctx, second); err != nil {
					t.Fatal(err)
				}
				if f.state != second {
					t.Fatalf("retry=%q", f.state)
				}
				if err := restarted.Remove(ctx); err != nil {
					t.Fatal(err)
				}
			} else if err := restarted.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if f.state != "original" {
				t.Fatalf("state=%q", f.state)
			}
		})
	}
}

func TestManagerRefreshProtectsExternalChangesBeforeAndDuringApply(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "during"}[during], func(t *testing.T) {
			ctx := context.Background()
			f := &fakeAdapter{state: "original"}
			m, _ := New(filepath.Join(t.TempDir(), "j"), f)
			if err := m.Install(ctx, "http://127.0.0.1:99/first.pac"); err != nil {
				t.Fatal(err)
			}
			if during {
				f.installState = "external"
				f.installErr = errors.New("apply failed")
			} else {
				f.state = "external"
			}
			if err := m.Refresh(ctx, "http://127.0.0.1:99/second.pac"); !errors.Is(err, ErrConflict) {
				t.Fatalf("refresh=%v", err)
			}
			if err := m.Recover(ctx); !errors.Is(err, ErrConflict) {
				t.Fatalf("recover=%v", err)
			}
			if f.state != "external" || f.restores != 0 {
				t.Fatalf("external setting overwritten: %+v", f)
			}
		})
	}
}

func (f *fakeAdapter) OwnsRestoration(ctx context.Context, prior json.RawMessage, pacURL, previousPACURL string) (bool, error) {
	matches, err := f.Matches(ctx, prior)
	if err != nil || matches {
		return matches, err
	}
	return f.OwnsTransition(ctx, pacURL, previousPACURL)
}

func TestManagerInitialInstallFailurePreservesExternalChanges(t *testing.T) {
	f := &fakeAdapter{state: "original", installErr: errors.New("partial apply failed"), installState: "external"}
	m, _ := New(filepath.Join(t.TempDir(), "j"), f)
	if err := m.Install(context.Background(), "http://127.0.0.1:99/first.pac"); !errors.Is(err, ErrConflict) {
		t.Fatalf("install=%v", err)
	}
	if err := m.Recover(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("recovery=%v", err)
	}
	if f.state != "external" || f.restores != 0 {
		t.Fatalf("external proxy overwritten: %+v", f)
	}
}

func TestManagerInitialInstallFailureRetainsRecoverableOriginalState(t *testing.T) {
	ctx := context.Background()
	f := &fakeAdapter{state: "original", installErr: errors.New("partial apply failed"), restoreErr: errors.New("restoration unavailable")}
	path := filepath.Join(t.TempDir(), "j")
	m, _ := New(path, f)
	pac := "http://127.0.0.1:99/first.pac"
	if err := m.Install(ctx, pac); err == nil {
		t.Fatal("expected failed install")
	}
	j, err := m.read()
	if err != nil || j.Phase != "restoring" {
		t.Fatalf("journal=%+v err=%v", j, err)
	}
	f.installErr, f.restoreErr = nil, nil
	restarted, _ := New(path, f)
	if err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if f.state != "original" {
		t.Fatalf("recovery=%q", f.state)
	}
	if err := restarted.Install(ctx, pac); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if f.state != "original" {
		t.Fatalf("retried install lost original state: %q", f.state)
	}
}

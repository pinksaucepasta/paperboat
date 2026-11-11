package enrollment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
)

// A renewal the server refuses with 401 will be refused again: the machine was removed or its credential revoked
// (prod 2026-11-11: six orphan daemons retried every ~65 s for hours). The refusal is recorded against the identity
// it was for, and this process attempts no renewal for that identity for renewalRefusalHold; a new enrollment writes a
// new credential and clears it by itself, and a restarted daemon makes one new attempt (see renewalRefused). Transient failures back off, doubling with jitter to a cap (tailscaled util/backoff; the
// shared schedule in shared/backoff), so a down control plane is not hit by every consumer on every call.
const (
	renewalRefusedFile    = "renewal-refused.json"
	renewalBackoffStart   = 5 * time.Second
	renewalBackoffMaximum = 2 * time.Minute // well inside RenewBefore: a credential still has time to be renewed
	// renewalRefusalHold is how long a refusal stops renewal in a process: an orphan then costs one request an hour
	// (not one a minute), and a machine refused only because its clock was off (the server answers 401 for a proof
	// outside its window) heals within the hour once the clock is right, without a re-enrollment.
	renewalRefusalHold = time.Hour
)

type renewalRefusal struct {
	HelperID         string    `json:"helper_id"`
	CredentialSHA256 string    `json:"credential_sha256"`
	At               time.Time `json:"at"`
}

func refusalFor(identity RuntimeIdentity) renewalRefusal {
	sum := sha256.Sum256([]byte(identity.Credential))
	return renewalRefusal{HelperID: identity.HelperID, CredentialSHA256: hex.EncodeToString(sum[:])}
}

// refusals and backoffs are per state root and per process: several token sources are built over one identity
// (the machine-control source builds one per recovery), and all of them share what the control plane said.
var (
	refusals sync.Map // state root -> renewalRefusal
	backoffs sync.Map // state root -> *renewalBackoff
	// backoffNow is the wall clock of the retry schedule; tests replace it.
	backoffNow = time.Now
)

func recordRenewalRefusal(root string, identity RuntimeIdentity, now time.Time) {
	refusal := refusalFor(identity)
	refusal.At = backoffNow().UTC()
	refusals.Store(root, refusal)
	if encoded, err := json.Marshal(refusal); err == nil { // best effort: the in-process record already stops this daemon
		_ = atomicfile.Write(filepath.Join(root, renewalRefusedFile), encoded, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
	}
}

// renewalRefused reports whether this process was refused a renewal for exactly this identity within
// renewalRefusalHold. Only the
// process remembers it: a 401 can also come from a proof the server judged expired (a wrong clock), which a daemon
// restart (or the clock being fixed) must be able to recover from, so the next process makes one new attempt.
func renewalRefused(root string, identity RuntimeIdentity) bool {
	recorded, ok := refusals.Load(root)
	if !ok {
		return false
	}
	want, got := refusalFor(identity), recorded.(renewalRefusal)
	return got.HelperID == want.HelperID && got.CredentialSHA256 == want.CredentialSHA256 && backoffNow().Before(got.At.Add(renewalRefusalHold))
}

func clearRenewalRefusal(root string) {
	refusals.Delete(root)
	_ = os.Remove(filepath.Join(root, renewalRefusedFile))
}

// RenewalRefused is what pb doctor asks (from another process, so it reads the file): the control plane refused this
// machine's renewal and the daemon stopped trying, so the machine must be enrolled again.
func RenewalRefused(root string, now time.Time) bool {
	identity, err := LoadRuntimeIdentityForRenewal(root, now)
	if err != nil {
		return false
	}
	body, err := os.ReadFile(filepath.Join(root, renewalRefusedFile))
	if err != nil || len(body) > 4096 {
		return false
	}
	var got renewalRefusal
	want := refusalFor(identity)
	return json.Unmarshal(body, &got) == nil && got.HelperID == want.HelperID && got.CredentialSHA256 == want.CredentialSHA256
}

// refusedRenewalError is the observed 401, returned without a request so callers classify it as before (a removed
// machine reads as refused, not as an unreachable control plane).
func refusedRenewalError() error {
	return errorreport.HTTPStatusFailure(&http.Response{StatusCode: http.StatusUnauthorized})
}

type renewalBackoff struct {
	mu       sync.Mutex
	failures int
	retryAt  time.Time
	last     error
}

func backoffFor(root string) *renewalBackoff {
	value, _ := backoffs.LoadOrStore(root, &renewalBackoff{})
	return value.(*renewalBackoff)
}

// wait returns the last failure while the retry window of the failed attempts is open, so no request is made. One
// failure opens no window (a blip is retried at once); the second and later consecutive ones do.
func (b *renewalBackoff) wait() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures > 1 && backoffNow().Before(b.retryAt) {
		return unavailableEnrollment(b.last)
	}
	return nil
}

func (b *renewalBackoff) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.last = err
	if b.failures > 1 {
		b.retryAt = backoffNow().Add(backoff.Retry(b.failures-1, renewalBackoffStart, renewalBackoffMaximum))
	}
}

func (b *renewalBackoff) succeed(root string) {
	clearRenewalRefusal(root)
	b.mu.Lock()
	b.failures, b.last, b.retryAt = 0, nil, time.Time{}
	b.mu.Unlock()
}

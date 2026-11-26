package paste

import (
	"strings"
	"testing"
	"time"
)

// A file dropped right after connecting arrives while the transfer lease and policy check are still running.
func TestDropDuringTransferSetupWaitsForItsOutcome(t *testing.T) {
	for _, test := range []struct {
		name     string
		bound    time.Duration
		outcome  func(*Policy, Setup)
		uploaded bool
	}{
		{name: "ready", bound: time.Minute, uploaded: true, outcome: func(_ *Policy, setup Setup) {
			setup.Ready(fixedUploader{"/vm/a.png"}, "ses_1", defaultLimits())
		}},
		{name: "failed", bound: time.Minute, outcome: func(_ *Policy, setup Setup) { setup.Settle() }},
		{name: "outlives the bound", bound: 50 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			img := makeImage(t, t.TempDir(), "a.png")
			policy := NewPolicy(nil, "ses_1", defaultLimits())
			setup := policy.Pending("ses_1", defaultLimits())
			dest, notice := &syncedBuffer{}, &syncedBuffer{}
			i := NewWithPolicy(dest, policy, WithNotifier(notice), WithPartialFlushDelay(time.Hour))
			i.setupTimeout = test.bound
			if _, err := i.Write([]byte(wrap(img))); err != nil {
				t.Fatal(err)
			}
			if test.outcome != nil {
				time.Sleep(20 * time.Millisecond)
				if got, warned := dest.String(), notice.String(); got != "" || warned != "" {
					t.Fatalf("drop did not wait for setup: pasted %q, notice %q", got, warned)
				}
				test.outcome(policy, setup)
			}
			if err := i.Close(); err != nil {
				t.Fatal(err)
			}
			want, warning := wrap(img), true
			if test.uploaded {
				want, warning = wrap("/vm/a.png"), false
			}
			if got := dest.String(); got != want {
				t.Fatalf("pasted %q, want %q", got, want)
			}
			if got := notice.String(); strings.Contains(got, "file transfer unavailable") != warning {
				t.Fatalf("notice = %q, want unavailable warning: %t", got, warning)
			}
		})
	}
}

// A reconnect replaces a setup that is still running: its late result must neither release a waiting drop nor install
// its uploader.
func TestReplacedTransferSetupChangesNothing(t *testing.T) {
	policy := NewPolicy(nil, "ses_1", defaultLimits())
	replaced := policy.Pending("ses_1", defaultLimits())
	current := policy.Pending("ses_1", defaultLimits())
	waiting := policy.snapshot().settled
	replaced.Ready(fixedUploader{"/vm/old.png"}, "ses_1", defaultLimits())
	replaced.Settle()
	select {
	case <-waiting:
		t.Fatal("a replaced setup released the waiting drop")
	default:
	}
	if policy.snapshot().transfer != nil {
		t.Fatal("a replaced setup installed its uploader")
	}
	current.Ready(fixedUploader{"/vm/new.png"}, "ses_1", defaultLimits())
	select {
	case <-waiting:
	default:
		t.Fatal("the current setup did not release the waiting drop")
	}
}

// A drop woken by one setup finds a reconnect already preparing the next one.
func TestDropKeepsWaitingForTheNextTransferSetup(t *testing.T) {
	policy := NewPolicy(nil, "ses_1", defaultLimits())
	first := policy.Pending("ses_1", defaultLimits())
	held := policy.snapshot()
	first.Ready(fixedUploader{"/vm/old.png"}, "ses_1", defaultLimits())
	next := policy.Pending("ses_1", defaultLimits())
	i := NewWithPolicy(&syncedBuffer{}, policy)
	t.Cleanup(func() { _ = i.Close() })
	woken := make(chan policySnapshot, 1)
	go func() {
		snapshot, _ := i.awaitSetup(held)
		woken <- snapshot
	}()
	select {
	case <-woken:
		t.Fatal("drop stopped waiting while the next setup was pending")
	case <-time.After(20 * time.Millisecond):
	}
	next.Ready(fixedUploader{"/vm/new.png"}, "ses_1", defaultLimits())
	if snapshot := <-woken; snapshot.transfer != (fixedUploader{"/vm/new.png"}) {
		t.Fatalf("uploader = %#v", snapshot.transfer)
	}
}

func TestAbortReleasesDropWaitingForTransferSetup(t *testing.T) {
	img := makeImage(t, t.TempDir(), "a.png")
	policy := NewPolicy(nil, "ses_1", defaultLimits())
	policy.Pending("ses_1", defaultLimits())
	dest := &syncedBuffer{}
	i := NewWithPolicy(dest, policy, WithPartialFlushDelay(time.Hour))
	if _, err := i.Write([]byte(wrap(img))); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		i.Abort()
		_ = i.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown waited for the pending transfer setup")
	}
}

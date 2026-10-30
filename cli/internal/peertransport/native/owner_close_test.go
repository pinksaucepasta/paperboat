package native

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/admission"
)

type shutdownKeys struct{}

func (shutdownKeys) Lookup(context.Context, string) (ed25519.PublicKey, bool, error) {
	return nil, false, nil
}

func TestOwnerShutdownInitiatesEverySessionBeforeWaitingForRelease(t *testing.T) {
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{
		Issuer: "https://control.example.test", Keys: shutdownKeys{},
		Self: tailnet.NetworkBinding{
			AccountID: "account", EndpointID: "cli", Role: "cli", EndpointGeneration: 1,
			QUICCertificateFingerprint: strings.Repeat("0", 64),
			QUICPublicKey:              base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	owner := &Owner{authority: authority, ctx: ctx, cancel: cancel, sessions: make(map[*Session]struct{})}
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	for range 3 {
		lease, err := admission.Admit(t.Context(), time.Time{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		lease.OnDone(func() { started <- struct{}{}; <-release })
		session := &Session{owner: owner, carrier: lease, quic: &peerquic.Session{}}
		owner.sessions[session] = struct{}{}
	}
	done := make(chan error, 1)
	go func() { done <- owner.Close() }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	allStarted := true
	for range 3 {
		select {
		case <-started:
		case <-deadline.C:
			allStarted = false
		}
		if !allStarted {
			break
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not join session release")
	}
	if !allStarted {
		t.Fatal("one session's shutdown delayed closing every other session")
	}
	if len(owner.sessions) != 0 {
		t.Fatal("owner returned with sessions still registered")
	}
}

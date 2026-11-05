//go:build linux || darwin

package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/carriercontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

func switchServer(t *testing.T, root, name, sha string, token []byte, release, start func(context.Context) error) string {
	t.Helper()
	ssh, err := managedssh.NewHost(managedssh.HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server := &carriercontrol.Server{Endpoint: filepath.Join(root, name), Token: token, SHA256: sha, SSH: ssh, Ready: make(chan error, 1), ReleaseNetwork: release, StartNetwork: start}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	return server.Endpoint
}

// The update hands the network over in one order: the serving carrier releases
// it, then the candidate starts it; a failed release never starts a second one.
func TestSwitchNetworkReleasesBeforeStartingAndStopsOnFailure(t *testing.T) {
	root := shorttemp.Dir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	token := []byte(strings.Repeat("c", 32))
	oldSHA, newSHA := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, releaseFails := range []bool{false, true} {
		var calls []string
		release := func(context.Context) error {
			calls = append(calls, "release")
			if releaseFails {
				return errors.New("stuck")
			}
			return nil
		}
		start := func(context.Context) error { calls = append(calls, "start"); return nil }
		name := map[bool]string{false: "ok", true: "bad"}[releaseFails]
		oldEndpoint := switchServer(t, root, name+"-old", oldSHA, token, release, nil)
		newEndpoint := switchServer(t, root, name+"-new", newSHA, token, nil, start)
		u := &unixCarrierUpgrade{previous: CarrierConfig{ControlEndpoint: oldEndpoint, Source: installsource.Source{SHA256: oldSHA}}, candidate: CarrierConfig{ControlEndpoint: newEndpoint, Source: installsource.Source{SHA256: newSHA}}, capability: append([]byte(nil), token...)}
		err := u.switchNetwork(t.Context())
		want := "release,start"
		if releaseFails {
			want = "release"
		}
		if got := strings.Join(calls, ","); got != want || (err != nil) != releaseFails {
			t.Fatalf("releaseFails=%v: calls %q err %v, want calls %q", releaseFails, got, err, want)
		}
	}
}

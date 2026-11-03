//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// terminalCarrier is a transport generation, not another machine or PTY owner.
// It runs in the already staged independent owner; stopping it leaves PTYs and
// their authenticated owner RPCs running. Only two usage/key-cache slots exist.
type terminalCarrier struct {
	slot          int
	offer         native.CarrierOffer
	network       *tailnet.CarrierNetwork
	run           context.Context
	owner         *native.Owner
	authority     *tailnet.Authority
	control       tailnet.NetworkAPI
	endpoint      *server.Server
	apps          *nativesession.Service
	connections   *sessionowner.Connections
	authorization Service
	usage         *hostBandwidthService
	unlock        func() error
	cancel        context.CancelFunc
	runDone       <-chan struct{}
	workers       sync.WaitGroup
	once          sync.Once
	closeErr      error
}

func newTerminalCarrier(ctx context.Context, version string, environ func(string) string, slot int, durable *store.Store) (_ *terminalCarrier, resultErr error) {
	if ctx == nil || environ == nil || durable == nil || slot < 1 || slot > 2 {
		return nil, ErrProductionInvalid
	}
	base, err := loadCarrierIdentity(ctx, version, environ)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("terminal-carrier-%d", slot)
	unlock, err := lockTerminalCarrier(filepath.Join(base.config.StateRoot, "runtime", name+".lock"))
	if err != nil {
		return nil, err
	}
	c := &terminalCarrier{slot: slot, unlock: unlock}
	defer func() {
		if resultErr != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, c.Close(cleanup))
		}
	}()
	issuer := strings.TrimRight(valueOrRuntime(environ("PAPERBOAT_CONTROL_ISSUER"), base.controlURL.String()), "/")
	keys, authorizer, authorization, recordJoin, err := terminalCarrierAuthorization(base, name, issuer, version)
	if err != nil {
		return nil, err
	}
	c.authorization = authorization
	tlsConfig, relayTLS, self, err := loadNativePeerTLS(base.config.StateRoot, base.identity.MachineID, uint64(base.generation))
	if err != nil {
		return nil, err
	}
	c.authority, err = tailnet.NewAuthority(tailnet.AuthorityOptions{Store: clientconfig.ProfileStore{Path: filepath.Join(base.config.StateRoot, "peer-network"), Secrets: clientconfig.FileSecretStore{Dir: filepath.Join(base.config.StateRoot, "peer-network", "secrets")}}, Issuer: issuer, Self: self, Keys: keys})
	if err != nil {
		return nil, err
	}
	control := clientapi.New(base.controlURL.String(), clientconfig.Credential{}, &http.Client{Transport: base.transport, Timeout: 15 * time.Second})
	control.SetMachineAuth(base.machineIdentity)
	c.control = terminalCarrierNetwork{control}
	if err := c.authority.Restore(ctx); err != nil {
		return nil, err
	}
	// Preparation must prove current authority. A control outage declines the
	// planned move and leaves today's primary-carrier reconnect path in charge.
	if err := c.authority.Refresh(ctx, c.control); err != nil {
		return nil, err
	}
	if _, err := c.authority.ConfigureRegionalRelays(relayTLS); err != nil {
		return nil, err
	}
	usage, err := bandwidth.Open(filepath.Join(base.config.StateRoot, name+"-bandwidth-usage.json"), control, newBandwidthObservation(ctx))
	if err != nil {
		return nil, err
	}
	c.usage = &hostBandwidthService{recorder: usage}
	var namespace [16]byte
	if _, err := rand.Read(namespace[:]); err != nil {
		return nil, err
	}
	c.owner, err = native.NewOwner(native.Config{Authority: c.authority, TLS: tlsConfig, PrepareCarrier: prepareTerminalCarrierPeer(base.config.StateRoot, environ("PAPERBOAT_SESSION_OWNER_CATALOG")), RefreshAuthority: func(ctx context.Context) error { return c.authority.RefreshForAdmission(ctx, c.control) }, MeterIncoming: func(conn net.Conn, binding native.MeterBinding) net.Conn {
		digest := sha256.Sum256(append(namespace[:], binding.StreamID...))
		return usage.Wrap(conn, bandwidth.Binding{AccessSessionID: binding.AccessSessionID, StreamID: fmt.Sprintf("stream_%x", digest), Consumer: binding.Consumer, Reverse: binding.Reverse}, func(direction string) bandwidth.Path {
			mode, node := binding.Path(direction)
			return bandwidth.Path{Mode: mode, NodeID: node}
		})
	}})
	if err != nil {
		return nil, err
	}
	c.connections, err = productionSessionOwners(ctx, base.config, environ)
	if err != nil {
		return nil, err
	}
	sessions, err := c.connections.Service()
	if err != nil {
		return nil, err
	}
	journal, err := operation.NewPersistentJournal(ctx, base.config.Resources.MaxConcurrentOps*32, durable, time.Hour, nil)
	if err != nil {
		return nil, err
	}
	c.endpoint, c.apps, err = NewTerminalCarrierEndpoint(TerminalCarrierEndpointConfig{Runtime: base.config, Sessions: sessions, Workspace: environ("PAPERBOAT_WORKSPACE_ROOT"), Shell: environ("PAPERBOAT_SHELL"), Authorizer: authorizer, Journal: journal, RecordTerminalJoin: recordJoin})
	if err != nil {
		return nil, err
	}
	if err := c.start(ctx, base); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *terminalCarrier) Close(ctx context.Context) error {
	c.once.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		if c.owner != nil {
			c.closeErr = c.owner.Close()
		} else if c.authority != nil {
			c.closeErr = c.authority.Close()
		}
		if c.network != nil {
			c.closeErr = errors.Join(c.closeErr, c.network.Close())
		}
		c.workers.Wait()
		if c.endpoint != nil {
			c.closeErr = errors.Join(c.closeErr, c.endpoint.Shutdown(ctx))
		}
		if c.apps != nil {
			c.apps.Wait()
		}
		if c.connections != nil {
			c.connections.Close()
		}
		if c.authorization != nil {
			c.closeErr = errors.Join(c.closeErr, c.authorization.Shutdown(ctx))
		}
		if c.usage != nil {
			c.closeErr = errors.Join(c.closeErr, c.usage.checkpoint(ctx))
		}
		c.closeErr = errors.Join(c.closeErr, c.unlock())
	})
	return c.closeErr
}

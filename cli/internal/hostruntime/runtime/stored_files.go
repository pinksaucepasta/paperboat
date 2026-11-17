//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	clientconfig "github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/environmentkey"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/filetransfer"
	runtimeidentity "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/cli/internal/storedfiles"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

type storedFileControl struct {
	client      *api.Client
	bucket      *http.Client
	keys        environmentkey.Source
	receiver    *filetransfer.Service
	machine     string
	operationID func() (string, error)
	events      *machineevents.Subscription
	mu          sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
}

func productionStoredFiles(base *url.URL, stateRoot string, transport http.RoundTripper, credentials api.MachineAuthSource, registration runtimeidentity.Registration, operationID func() (string, error), events *machineevents.Hub) func(*filetransfer.Service) (Service, error) {
	return func(receiver *filetransfer.Service) (Service, error) {
		keys, err := productionEnvironmentKeySourceForState(stateRoot, registration)
		if err != nil {
			return nil, err
		}
		client := api.New(base.String(), clientconfig.Credential{}, &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: rejectRuntimePolicyRedirect})
		client.SetMachineAuth(credentials)
		return &storedFileControl{client: client, bucket: &http.Client{Timeout: 90 * time.Second, CheckRedirect: rejectRuntimePolicyRedirect}, keys: keys, receiver: receiver, machine: registration.MachineID, operationID: operationID, events: events.Subscribe(machineevents.TopicStoredFiles), done: make(chan struct{})}, nil
	}
}

func (c *storedFileControl) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx == nil || c.cancel != nil {
		return ErrProductionInvalid
	}
	run, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go func() {
		defer close(c.done)
		failures := 0
		for {
			pass, cancel := context.WithTimeout(run, 15*time.Minute)
			err := c.pull(pass)
			cancel()
			interval := 30 * time.Second
			if err != nil {
				failures++
				interval = backoff.Retry(failures, 30*time.Second, 15*time.Minute)
				diagnosticlog.TryWarn("stored files could not be delivered; retrying")
			} else {
				failures = 0
			}
			if !c.events.Wait(run, interval) {
				return
			}
		}
	}()
	return nil
}
func (c *storedFileControl) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return nil
	}
}
func (c *storedFileControl) pull(ctx context.Context) error {
	operation, err := c.operationID()
	if err != nil {
		return err
	}
	page, err := c.client.MachineStoredFiles(ctx, operation)
	if err != nil {
		return err
	}
	if len(page.Transfers) == 0 {
		return nil
	}
	material, err := c.keys.Load(ctx)
	if err != nil {
		return err
	}
	defer material.Destroy()
	var failures []error
	for _, d := range page.Transfers {
		raw := d.Metadata.SealedKeys[c.machine]
		key, err := environmente2ee.OpenStoredFileKey(ctx, raw, d.Metadata.WriterPublic, material.Private[:], d.RequestID, d.ManifestDigest, c.machine)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		err = storedfiles.Pull(ctx, storedfiles.PullConfig{RequestID: d.RequestID, ManifestDigest: d.ManifestDigest, MachineID: c.machine,
			SourceMachineID: d.SourceMachineID, SenderAccount: d.SenderAccount, Files: d.Files, Metadata: d.Metadata, Key: key, Receiver: c.receiver, Client: c.bucket,
			URLs: func(ctx context.Context, first, count int) ([]teambucket.Authority, error) {
				op, err := c.operationID()
				if err != nil {
					return nil, err
				}
				urls, err := c.client.MachineStoredDownloads(ctx, op, d.RequestID, first, count)
				return urls.URLs, err
			}})
		clear(key)
		if err == nil {
			var op string
			op, err = c.operationID()
			if err == nil {
				err = c.client.MachineStoredReceived(ctx, op, d.RequestID, d.ManifestDigest)
			}
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

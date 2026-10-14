package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat-helper/internal/containers"
	"github.com/pinksaucepasta/paperboat-relay/selfhost"
)

const serviceCommandTimeout = 30 * time.Second

func installRuntimeImages(ctx context.Context, dir string, c selfhost.Config, images containers.Images, noService bool) error {
	if noService {
		return containers.SaveImages(dir, images)
	}
	old, oldErr := containers.LoadImages(dir)
	if oldErr != nil && !errors.Is(oldErr, os.ErrNotExist) {
		return oldErr
	}
	claimed, err := selfhost.RuntimeReady(dir)
	if err != nil {
		return err
	}
	var before map[string]string
	if claimed {
		snapshot, cancel := context.WithTimeout(ctx, 15*time.Second)
		before, err = containers.InstallationEpochs(snapshot, dir)
		cancel()
		if err != nil {
			return fmt.Errorf("%w: %w", commandError("read existing runtime readiness failed; installation unchanged"), err)
		}
	}
	if err := containers.SaveImages(dir, images); err != nil {
		return err
	}
	if err := restartInstallation(ctx, dir, c, claimed, before); err == nil {
		return nil
	} else {
		primary := fmt.Errorf("%w: %w", commandError("runtime upgrade failed; restoring previous images"), err)
		recovery, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if oldErr != nil {
			stopErr := stopInstallation(recovery)
			return errors.Join(primary, stopErr)
		}
		if stopErr := stopInstallation(recovery); stopErr != nil {
			return errors.Join(primary, stopErr)
		}
		if saveErr := containers.SaveImages(dir, old); saveErr != nil {
			return errors.Join(primary, saveErr)
		}
		if recoveryErr := restartInstallation(recovery, dir, c, claimed, before); recoveryErr != nil {
			return errors.Join(primary, fmt.Errorf("%w: %w", commandError("previous images restored but service recovery failed; restart the service"), recoveryErr))
		}
		return primary
	}
}

func restartInstallation(ctx context.Context, dir string, c selfhost.Config, claimed bool, previous map[string]string) error {
	if err := installService(ctx); err != nil {
		return err
	}
	ready, cancel := context.WithTimeout(ctx, containers.StartupTimeout)
	defer cancel()
	if err := waitClaimTLS(ready, dir, c); err != nil {
		return err
	}
	if claimed {
		return containers.WaitInstallationReady(ready, dir, previous)
	}
	return nil
}

func waitClaimTLS(ctx context.Context, dir string, c selfhost.Config) error {
	certificate, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return errors.New("installation TLS certificate is invalid")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 2 * time.Second}, Config: &tls.Config{RootCAs: roots, ServerName: c.EndpointHost, MinVersion: tls.VersionTLS13}}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err == nil {
			connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("installation claim TLS listener did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func stopInstallation(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, serviceCommandTimeout)
	defer cancel()
	return exec.CommandContext(bounded, "systemctl", "stop", "paperboat-selfhost.service").Run()
}

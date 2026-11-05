package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/server/internal/servernodes"
)

// runEnrollNode is a new server node before it holds any credentials: it prints a one-time claim code,
// waits for an operator to claim it from any node's panel, stores the delivered config and exits 0. The
// process supervisor then runs `serve --config <config-out>`.
func runEnrollNode(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("enroll-node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	region := fs.String("region", "", "region name, for example mumbai")
	instance := fs.String("instance", "", "instance slot in the region, slot-01 to slot-64")
	host := fs.String("host", "", "host name or IP address operators reach this node at")
	public := fs.String("public-address", "", "public IP address DNS publishes for this node (default: -host)")
	listen := fs.String("listen", ":8443", "address to accept the claim on")
	out := fs.String("config-out", "", "absolute path to store the delivered config (owner-only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u, code, err := servernodes.NewUnenrolled(*region, *instance, *host, *public, buildinfo.Version, buildinfo.Commit, *out)
	if err != nil {
		return fmt.Errorf("prepare enrollment: %w (need -region, -instance slot-NN, -host, -config-out and a public -public-address)", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen for the claim on %s: %w", *listen, err)
	}
	srv := &http.Server{Handler: u, TLSConfig: u.TLSConfig(), ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(ln, "", "") }()
	defer srv.Close()
	fmt.Fprintf(stdout, "Claim code (valid %s): %s\nIn the operator panel of any server node, enroll %s with this code.\n",
		servernodes.CodeLife, code, net.JoinHostPort(*host, portOf(ln)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-u.Claimed():
		fmt.Fprintf(stdout, "Enrolled. Config stored at %s; start the node with: serve --config %s\n", *out, *out)
		return nil
	case <-time.After(servernodes.CodeLife):
		return errors.New("the claim code expired before an operator used it; run enroll-node again for a new code")
	case err := <-served:
		return fmt.Errorf("serve the claim endpoint: %w", err)
	case <-ctx.Done():
		return errors.New("enrollment stopped before an operator claimed this node")
	}
}

func portOf(ln net.Listener) string {
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

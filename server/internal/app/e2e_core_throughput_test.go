package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"time"
)

const (
	throughputMiB    = 200
	throughputBudget = 150.0 // Mbit/s, PR-06: one tunnel stream at 50 ms RTT
)

// assertTunnelThroughput is PR-06 on a quiet runner (PAPERBOAT_E2E_THROUGHPUT=1, e2e.yml dispatch input throughput):
// one tunnel stream through the real edge sustains throughputBudget at 50 ms RTT. The delay is delayOrigin's netem
// on the origin container; the visitor is this process, on the host next to the edge.
func (e *e2eCore) assertTunnelThroughput() {
	origin := e.machines[1]
	var hostname string
	e.step("throughput: origin serves a file", func() error {
		_, err := e.run(2*time.Minute, "docker", "exec", "-u", "pbt", origin, "bash", "-c", fmt.Sprintf(
			"mkdir -p /tmp/8082 && head -c %d /dev/urandom >/tmp/8082/big && cd /tmp/8082 && (setsid python3 -m http.server 8082 >/dev/null 2>&1 &)", throughputMiB<<20))
		return err
	})
	e.step("throughput: tunnel published", func() error {
		return waitFor(2*time.Minute, func() error {
			out, err := e.pb(origin, "tunnel", "8082", "--ephemeral", "--background", "--json")
			if err != nil {
				return err
			}
			if hostname = publicHostname(out); hostname == "" {
				return fmt.Errorf("pb tunnel printed no public URL: %s", lastBytes(out, 400))
			}
			return nil
		})
	})
	client, err := e.clientTrusting(filepath.Join(e.work, "pebble-roots.pem"), func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(e.gateway, "443"))
	})
	if err != nil {
		e.t.Fatal(err)
	}
	client.Timeout = 5 * time.Minute
	e.step("throughput: tunnel serves", func() error {
		return waitFor(2*time.Minute, func() error { return getOK(client, "https://"+hostname+"/") })
	})
	var rates []float64
	e.step("throughput: three downloads", func() error {
		for range 3 {
			start := time.Now()
			response, err := client.Get("https://" + hostname + "/big")
			if err != nil {
				return err
			}
			n, err := io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || n != throughputMiB<<20 {
				return fmt.Errorf("download: %s, %d bytes, %v", response.Status, n, err)
			}
			rates = append(rates, float64(n)*8/time.Since(start).Seconds()/1e6)
		}
		return nil
	})
	sort.Float64s(rates)
	e.t.Logf("tunnel throughput at +50 ms: %.1f Mbit/s median of %.1f (budget %.0f)", rates[1], rates, throughputBudget)
	if rates[1] < throughputBudget {
		e.t.Fatalf("tunnel throughput at +50 ms is %.1f Mbit/s median (runs %.1f), budget %.0f", rates[1], rates, throughputBudget)
	}
}

// delayOrigin puts 50 ms on the origin container's egress before it pairs, so every QUIC connection it makes
// handshakes at the real RTT. A connection made before the delay keeps its ~1 ms minimum RTT and quic-go's HyStart
// reads each later 50 ms sample as a delay increase and leaves slow start at once (a 40 KB window, 16 KB/s growth:
// 88-95 Mbit/s measured instead of 180). limit 100000: netem's default 1000-packet queue drops the flow's bursts.
func (e *e2eCore) delayOrigin(machine string) error {
	_, err := e.docker(machine, "tc", "qdisc", "replace", "dev", "eth0", "root", "netem", "delay", "50ms", "limit", "100000")
	return err
}

package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	throughputMiB    = 200
	throughputBudget = 150.0 // Mbit/s, PR-06: one tunnel stream at 50 ms RTT
)

// assertTunnelThroughput is PR-06 on a quiet runner (PAPERBOAT_E2E_THROUGHPUT=1, e2e.yml dispatch input throughput):
// one tunnel stream through the real edge sustains throughputBudget at 50 ms RTT. The delay is netem on the origin
// container's egress, applied before the carrier restarts so the carrier measures it (yamux/QUIC windows grow from
// the RTT they see); limit 100000 because netem's default 1000-packet queue drops the flow's bursts and the loss,
// not the carrier, then caps the rate. The visitor is this process, on the host next to the edge.
func (e *e2eCore) assertTunnelThroughput() {
	origin := e.machines[1]
	var hostname string
	e.step("throughput: origin serves a file", func() error {
		_, err := e.run(2*time.Minute, "docker", "exec", "-u", "pbt", origin, "bash", "-c", fmt.Sprintf(
			"mkdir -p /tmp/8082 && head -c %d /dev/urandom >/tmp/8082/big && cd /tmp/8082 && (setsid python3 -m http.server 8082 >/dev/null 2>&1 &)", throughputMiB<<20))
		return err
	})
	e.step("throughput: +50 ms on the origin's egress, carrier restarted", func() error {
		if _, err := e.docker(origin, "tc", "qdisc", "replace", "dev", "eth0", "root", "netem", "delay", "50ms", "limit", "100000"); err != nil {
			return err
		}
		if os.Getenv("PAPERBOAT_E2E_THROUGHPUT_TCP") != "" { // the TCP carrier: without UDP to the edge the carrier falls back to it (40 s below)
			if _, err := e.docker(origin, "iptables", "-I", "OUTPUT", "-p", "udp", "-d", e.gateway, "-j", "DROP"); err != nil {
				return err
			}
		}
		_, err := e.docker(origin, "bash", "-c", "systemctl restart 'paperboat-carrier-u1000-*.service'")
		time.Sleep(40 * time.Second)
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
	before := udpCounters()
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
	loss, _ := e.docker(origin, "tc", "-s", "qdisc", "show", "dev", "eth0")
	e.t.Logf("origin netem after the downloads (dropped > 0 = loss before the edge):\n%s", loss)
	e.t.Logf("host UDP counters during the downloads: %v (RcvbufErrors > 0 = the edge's UDP receive buffer overflowed)", udpCounters().since(before))
	e.t.Logf("tunnel throughput at +50 ms: %.1f Mbit/s median of %.1f (budget %.0f)", rates[1], rates, throughputBudget)
	if rates[1] < throughputBudget {
		e.t.Fatalf("tunnel throughput at +50 ms is %.1f Mbit/s median (runs %.1f), budget %.0f", rates[1], rates, throughputBudget)
	}
}

type udpStats map[string]int

// udpCounters reads the host's UDP counters (/proc/net/snmp): the edge listens on the host.
func udpCounters() udpStats {
	raw, err := os.ReadFile("/proc/net/snmp")
	if err != nil {
		return nil
	}
	var names []string
	stats := udpStats{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Udp:" {
			continue
		}
		if names == nil {
			names = fields[1:]
			continue
		}
		for i, v := range fields[1:] {
			stats[names[i]], _ = strconv.Atoi(v)
		}
	}
	return stats
}

func (s udpStats) since(before udpStats) udpStats {
	delta := udpStats{}
	for k, v := range s {
		if v != before[k] {
			delta[k] = v - before[k]
		}
	}
	return delta
}

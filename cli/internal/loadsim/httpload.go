package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type httpFlags struct {
	edge, run, domain, mode, rates, path string
	hosts, conns, maxInflight            int
	duration                             time.Duration
	bytes                                int64
	h1, newConn                          bool
}

// httpLoad drives the edge's public listener like browsers do: TLS with the route's host name as SNI, HTTP/2.
//
//	small: open-loop request rates (-rates 500,1000,...), each held for -duration; one JSON line per rate with the latency percentiles.
//	large: -conns workers downloading /big?bytes=N back to back for -duration; one JSON line with Mbit/s.
func httpLoad(args []string) error {
	var f httpFlags
	flags := flag.NewFlagSet("http", flag.ExitOnError)
	flags.StringVar(&f.edge, "edge", "127.0.0.1:443", "edge public HTTPS address")
	flags.StringVar(&f.run, "run", "a", "run id the machines were started with")
	flags.StringVar(&f.domain, "domain", "preview.example.test", "endpoint route base domain")
	flags.StringVar(&f.mode, "mode", "small", "small or large")
	flags.StringVar(&f.rates, "rates", "500,1000,2000,4000", "small: requests per second to try, in order")
	flags.StringVar(&f.path, "path", "/", "small: request path")
	flags.IntVar(&f.hosts, "hosts", 100, "route names in use (machines x routes), requests pick one at random")
	flags.IntVar(&f.conns, "conns", 32, "TCP connections to the edge")
	flags.IntVar(&f.maxInflight, "max-inflight", 20000, "small: requests in flight before further ones count as dropped")
	flags.DurationVar(&f.duration, "duration", 20*time.Second, "time per step")
	flags.Int64Var(&f.bytes, "bytes", 64<<20, "large: bytes per download")
	flags.BoolVar(&f.newConn, "new-conn", false, "a new TCP+TLS connection for every request (handshake cost)")
	flags.BoolVar(&f.h1, "h1", false, "use HTTP/1.1 instead of HTTP/2")
	if err := flags.Parse(args); err != nil {
		return err
	}
	clients := make([]*http.Client, f.conns)
	for i := range clients {
		// The edge limits requests, handshakes and streams per client address (edgehttp/ip_limits.go); every client connection gets its own
		// loopback source address, as a crowd of visitors would, so the run measures the node and not that abuse limit.
		local := &net.TCPAddr{IP: net.IPv4(127, 1, byte(i>>8), byte(i&255)+1)}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 10 * time.Second, LocalAddr: local}).DialContext(ctx, network, f.edge)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: !f.h1, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: time.Minute, DisableKeepAlives: f.newConn,
		}
		if f.h1 {
			transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		}
		clients[i] = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	}
	// Like browsers, each connection serves one host: client i is bound to route i%hosts, so -conns above -hosts shares routes.
	request := func(path string) (int, int64, error) {
		i := rand.IntN(len(clients))
		host := fmt.Sprintf("r%s%d.%s", f.run, i%f.hosts, f.domain)
		req, err := http.NewRequest("GET", "https://"+host+path, nil)
		if err != nil {
			return 0, 0, err
		}
		response, err := clients[i].Do(req)
		if err != nil {
			return 0, 0, err
		}
		n, err := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode, n, err
	}
	switch f.mode {
	case "small":
		for _, text := range strings.Split(f.rates, ",") {
			rate, err := strconv.Atoi(strings.TrimSpace(text))
			if err != nil || rate < 1 {
				return fmt.Errorf("bad rate %q", text)
			}
			printJSON(smallStep(f, rate, request))
		}
	case "large":
		printJSON(largeStep(f, request))
	default:
		return fmt.Errorf("unknown mode %q", f.mode)
	}
	return nil
}

func printJSON(value map[string]any) {
	data, _ := json.Marshal(value)
	fmt.Println(string(data))
	_ = os.Stdout.Sync()
}

func smallStep(f httpFlags, rate int, request func(string) (int, int64, error)) map[string]any {
	var mu sync.Mutex
	var latencies []time.Duration
	var sent, dropped, failed, non200 atomic.Int64
	errorsSeen := map[string]int{}
	var inflight atomic.Int64
	var wg sync.WaitGroup
	started := time.Now()
	interval := time.Second / time.Duration(rate)
	next := started
	for time.Since(started) < f.duration {
		if sleep := time.Until(next); sleep > 0 {
			//paperboat:allow-source-policy sleep owner=capacity-harness reason=open-loop-request-pacing-not-a-retry
			time.Sleep(sleep)
		}
		// Open loop: a slow edge does not slow the sender; catch up on the schedule instead.
		for next.Before(time.Now()) && time.Since(started) < f.duration {
			next = next.Add(interval)
			if inflight.Load() >= int64(f.maxInflight) {
				dropped.Add(1)
				continue
			}
			sent.Add(1)
			inflight.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer inflight.Add(-1)
				begin := time.Now()
				status, _, err := request(f.path)
				took := time.Since(begin)
				switch {
				case err != nil:
					failed.Add(1)
					text := err.Error()
					if len(text) > 100 {
						text = text[len(text)-100:]
					}
					mu.Lock()
					if len(errorsSeen) < 8 || errorsSeen[text] > 0 {
						errorsSeen[text]++
					}
					mu.Unlock()
				case status != 200:
					non200.Add(1)
					mu.Lock()
					errorsSeen["status "+strconv.Itoa(status)]++
					mu.Unlock()
				default:
					mu.Lock()
					latencies = append(latencies, took)
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(started).Seconds()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(p float64) float64 {
		if len(latencies) == 0 {
			return -1
		}
		return float64(latencies[int(float64(len(latencies)-1)*p)].Microseconds()) / 1000
	}
	return map[string]any{"mode": "small", "target_rps": rate, "achieved_rps": float64(len(latencies)) / elapsed, "sent": sent.Load(), "ok": len(latencies), "failed": failed.Load(),
		"non200": non200.Load(), "dropped": dropped.Load(), "p50_ms": percentile(0.5), "p99_ms": percentile(0.99), "p999_ms": percentile(0.999), "max_ms": percentile(1), "errors": errorsSeen, "conns": f.conns, "seconds": elapsed}
}

func largeStep(f httpFlags, request func(string) (int, int64, error)) map[string]any {
	var total, failed, done atomic.Int64
	var wg sync.WaitGroup
	started := time.Now()
	deadline := started.Add(f.duration)
	path := "/big?bytes=" + strconv.FormatInt(f.bytes, 10)
	for i := 0; i < f.conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				status, n, err := request(path)
				total.Add(n)
				if err != nil || status != 200 {
					failed.Add(1)
					//paperboat:allow-source-policy sleep owner=capacity-harness reason=open-loop-request-pacing-not-a-retry
					time.Sleep(100 * time.Millisecond)
					continue
				}
				done.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started).Seconds()
	return map[string]any{"mode": "large", "conns": f.conns, "bytes_each": f.bytes, "seconds": elapsed, "downloads": done.Load(), "failed": failed.Load(),
		"mbit_s": float64(total.Load()) * 8 / 1e6 / elapsed, "bytes": total.Load()}
}

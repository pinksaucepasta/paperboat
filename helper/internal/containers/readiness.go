package containers

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

type NodeReadiness struct {
	NodeID       string     `json:"node_id"`
	Capability   string     `json:"capability"`
	Ready        bool       `json:"ready"`
	ProcessEpoch string     `json:"process_epoch"`
	ServerTime   time.Time  `json:"server_time"`
	ValidUntil   *time.Time `json:"valid_until"`
}

type ResponseError struct{ Status int }

func (e ResponseError) Error() string {
	return "check node readiness: server returned HTTP" + strconv.Itoa(e.Status)
}

type nodeClient struct {
	url, credential, nodeID, capability string
	http                                *http.Client
}

func newNodeClient(capability, dir string) (*nodeClient, error) {
	args, _, err := selfhost.RuntimeArgs([]string{"run", "--state-dir", dir})
	if err != nil {
		return nil, err
	}
	get := func(flag string) string {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag {
				return args[i+1]
			}
		}
		return ""
	}
	node := &nodeClient{nodeID: get("-node-id"), capability: capability, url: get("-control-url")}
	caPath := get("-control-ca")
	if capability == "tunnel" {
		var d struct {
			ControlURL string `json:"control_url"`
			ControlCA  string `json:"control_ca_file"`
		}
		file, err := os.Open(filepath.Join(dir, "deployment.json"))
		if err != nil {
			return nil, err
		}
		err = json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&d)
		file.Close()
		if err != nil {
			return nil, errors.New("read runtime control configuration failed")
		}
		node.url, caPath = d.ControlURL, d.ControlCA
	}
	parsed, err := url.Parse(node.url)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || node.nodeID == "" {
		return nil, errors.New("runtime control address or node identity is invalid")
	}
	credentialPath := filepath.Join(dir, "runtime.credential")
	info, err := os.Lstat(credentialPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return nil, errors.New("runtime credential must be a bounded owner-only regular file")
	}
	credential, err := os.ReadFile(credentialPath)
	if err != nil {
		return nil, err
	}
	defer clear(credential)
	if len(credential) > 4096 {
		return nil, errors.New("runtime credential is oversized")
	}
	node.credential = strings.TrimSpace(string(credential))
	if len(node.credential) < 32 {
		return nil, errors.New("runtime credential is missing")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("runtime control CA is invalid")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	node.http = &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return node, nil
}

func (n *nodeClient) readiness(ctx context.Context) (NodeReadiness, error) {
	var result NodeReadiness
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(n.url, "/")+"/v1/selfhost/runtime-readiness", nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+n.credential)
	response, err := n.http.Do(req)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return result, ResponseError{Status: response.StatusCode}
	}
	var wire struct {
		Data NodeReadiness `json:"data"`
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16385))
	if err != nil || len(body) > 16384 {
		return result, errors.New("node readiness response exceeded its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&wire); err != nil {
		return result, errors.New("node readiness response is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, errors.New("node readiness response is oversized or has trailing data")
	}
	result = wire.Data
	if result.NodeID != n.nodeID || result.Capability != n.capability || result.ServerTime.IsZero() || (result.Ready && (result.ProcessEpoch == "" || result.ValidUntil == nil || !result.ValidUntil.After(result.ServerTime))) {
		return NodeReadiness{}, errors.New("node readiness identity or freshness is invalid")
	}
	return result, nil
}

// PortProbe is the control plane's view of one test port on this node's public address.
type PortProbe struct {
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail"`
}

// ProbePort asks the control plane to connect to this node's public address on port and expect nonce
// echoed back by a listener the caller has opened there. dir is the installation state directory; the
// tunnel component's runtime files live in its "tunnel" subdirectory.
func ProbePort(ctx context.Context, dir string, port int, nonce string) (PortProbe, error) {
	node, err := newNodeClient("tunnel", filepath.Join(dir, "tunnel"))
	if err != nil {
		return PortProbe{}, err
	}
	body, err := json.Marshal(map[string]any{"port": port, "nonce": nonce})
	if err != nil {
		return PortProbe{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(node.url, "/")+"/v1/selfhost/port-probe", bytes.NewReader(body))
	if err != nil {
		return PortProbe{}, err
	}
	req.Header.Set("Authorization", "Bearer "+node.credential)
	req.Header.Set("Content-Type", "application/json")
	node.http.Timeout = 15 * time.Second
	response, err := node.http.Do(req)
	if err != nil {
		return PortProbe{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return PortProbe{}, ResponseError{Status: response.StatusCode}
	}
	var wire struct {
		Data PortProbe `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&wire); err != nil || wire.Data.Detail == "" {
		return PortProbe{}, errors.New("port probe response is invalid")
	}
	return wire.Data, nil
}

package auditstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// httpDoer sends one request; publicClient in production.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ErrPrivateAddress means a destination may only be a public internet address, never this server's
// network (no SSRF into the platform).
var ErrPrivateAddress = errors.New("the destination must be a public internet address")

// publicClient refuses, at connect time, any address that is not a public unicast address, so a
// DNS answer cannot steer delivery into private networks. Redirects are not followed.
func publicClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if !publicIP(net.ParseIP(host)) {
			return ErrPrivateAddress
		}
		return nil
	}}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout, MaxIdleConnsPerHost: 2}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func publicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!ip.Equal(net.IPv4(100, 100, 100, 200)) && !inCGNAT(ip)
}

// inCGNAT covers 100.64.0.0/10, used by Tailscale and carrier NAT.
func inCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64
}

// DeliveryError says why a destination refused a batch; Status is 0 for network failures.
type DeliveryError struct {
	Status int
	Reason string
}

func (e *DeliveryError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("the destination answered HTTP %d", e.Status)
	}
	return e.Reason
}

func deliveryFailure(err error) error {
	if errors.Is(err, ErrPrivateAddress) {
		return &DeliveryError{Reason: ErrPrivateAddress.Error()}
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return &DeliveryError{Reason: "the destination did not answer in time"}
	}
	return &DeliveryError{Reason: "could not connect to the destination"}
}

// SignatureHeader carries t=<unix seconds>,v1=<hex HMAC-SHA256 of "t.body">. Receivers recompute
// it with their signing secret and reject old timestamps (as Stripe does). It stays outside the
// Paperboat-* namespace, which proxies such as the Paperboat tunnel edge strip as credentials.
const SignatureHeader = "Pb-Signature"

// signature is the SignatureHeader value.
func signature(secret string, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(t + "."))
	m.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

// sendHTTPS POSTs one NDJSON batch.
func sendHTTPS(ctx context.Context, client httpDoer, d Destination, secrets Secrets, body []byte, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return &DeliveryError{Reason: "invalid destination URL"}
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("User-Agent", "Paperboat-Audit-Stream/1")
	req.Header.Set(SignatureHeader, signature(secrets.SigningSecret, now, body))
	if d.HeaderName != "" {
		req.Header.Set(d.HeaderName, secrets.HeaderValue)
	}
	res, err := client.Do(req)
	if err != nil {
		return deliveryFailure(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &DeliveryError{Status: res.StatusCode}
	}
	return nil
}

// objectKey names a batch: <prefix>/<workspace>/YYYY/MM/DD/<first>-<last>.ndjson.gz. The same
// batch always gets the same name, so a retried upload overwrites rather than duplicates.
func objectKey(prefix, workspace string, at time.Time, first, last int64) string {
	key := fmt.Sprintf("%s/%s/%d-%d.ndjson.gz", strings.ReplaceAll(workspace, ":", "-"), at.UTC().Format("2006/01/02"), first, last)
	if p := strings.Trim(prefix, "/"); p != "" {
		key = p + "/" + key
	}
	return key
}

// objectURL addresses the object path-style (https://endpoint/bucket/key), which every
// S3-compatible store supports, with the path encoded the way SigV4 signs it.
func objectURL(endpoint, bucket, key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, &DeliveryError{Reason: "the endpoint must be an https URL"}
	}
	u.Path = "/" + bucket + "/" + key
	u.RawPath = canonicalPath(u.Path)
	return u, nil
}

// sendS3 uploads one gzip NDJSON batch with a SigV4-signed PUT.
func sendS3(ctx context.Context, client httpDoer, d Destination, secrets Secrets, body []byte, key string, now time.Time) error {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	u, err := objectURL(d.URL, d.Bucket, key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(gz.Bytes()))
	if err != nil {
		return &DeliveryError{Reason: "invalid destination"}
	}
	req.Host = u.Host
	req.ContentLength = int64(gz.Len())
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	signV4(req, d.AccessKeyID, secrets.SecretAccessKey, d.Region, hexSHA(gz.Bytes()), now)
	res, err := client.Do(req)
	if err != nil {
		return deliveryFailure(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &DeliveryError{Status: res.StatusCode}
	}
	return nil
}

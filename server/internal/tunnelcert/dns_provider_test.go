package tunnelcert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type testSecretSource struct {
	value []byte
}

func (s testSecretSource) Resolve(context.Context, string) ([]byte, error) {
	return append([]byte(nil), s.value...), nil
}

type testTXTResolver struct {
	mu     sync.Mutex
	values [][]string
}

func (r *testTXTResolver) LookupTXT(context.Context, string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.values) == 0 {
		return nil, nil
	}
	value := r.values[0]
	if len(r.values) > 1 {
		r.values = r.values[1:]
	}
	return value, nil
}

type failingTXTResolver struct{ err error }

func (r failingTXTResolver) LookupTXT(context.Context, string) ([]string, error) {
	return nil, r.err
}

type waitingTXTResolver struct{ started chan struct{} }

func (r waitingTXTResolver) LookupTXT(ctx context.Context, _ string) ([]string, error) {
	r.started <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCloudflareDNSProviderUsesWriteOnlyReferenceAndCleansUp(t *testing.T) {
	const token = "cloudflare-secret-token"
	var gotAuth string
	var gotBody struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		TTL     int    `json:"ttl"`
		Proxied bool   `json:"proxied"`
	}
	var deletePath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatal(err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"record_123"}}`))
			return
		}
		deletePath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":null}`))
	}))
	defer server.Close()
	resolver := &testTXTResolver{values: [][]string{nil, {"expected-value"}}}
	provider, err := NewCloudflareDNSProvider(CloudflareDNSConfig{BaseURL: server.URL + "/client/v4/", ZoneID: "zone_123", TokenReference: "secret/cloudflare", TokenSource: testSecretSource{value: []byte(token)}, Resolver: resolver, PropagationWait: 100 * time.Millisecond, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	record, err := provider.Present(context.Background(), DNS01Record{Domain: "preview.example.test", Name: "_acme-challenge.preview.example.test", Value: "expected-value", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if record.ProviderID != "record_123" || gotAuth != "Bearer "+token || gotBody.Type != "TXT" || gotBody.Name != "_acme-challenge.preview.example.test" || gotBody.Content != "expected-value" || gotBody.TTL != 60 || gotBody.Proxied {
		t.Fatalf("record=%+v auth=%q body=%+v", record, gotAuth, gotBody)
	}
	if err := provider.Wait(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := provider.Cleanup(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if deletePath != "/client/v4/zones/zone_123/dns_records/record_123" {
		t.Fatalf("delete path=%q", deletePath)
	}
}

func TestCloudflareDNSProviderNeverReturnsCredentialOrResponseBody(t *testing.T) {
	const token = "sensitive-cloudflare-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"message":"` + token + `"}]}`))
	}))
	defer server.Close()
	provider, err := NewCloudflareDNSProvider(CloudflareDNSConfig{BaseURL: server.URL, ZoneID: "zone_123", TokenReference: "secret/cloudflare", TokenSource: testSecretSource{value: []byte(token)}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Present(context.Background(), DNS01Record{Domain: "preview.example.test", Name: "_acme-challenge.preview.example.test", Value: "value", TTL: time.Minute})
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "success") {
		t.Fatalf("unsafe provider error=%v", err)
	}
}

func TestCloudflareDNSProviderPreservesRequestCausesWithoutExposingText(t *testing.T) {
	const privateCauseText = "private transport diagnostic"
	transportCause := errors.New(privateCauseText)
	readCause := errors.New("private response read diagnostic")
	closeCause := errors.New("private response close diagnostic")
	validBody := `{"success":true,"result":{"id":"record_123"}}`
	tests := []struct {
		name      string
		transport func(*http.Request) (*http.Response, error)
		cause     error
		status    int
		wantCalls int
		wantJSON  bool
	}{
		{
			name: "exhausted transport retries",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, transportCause
			},
			cause: transportCause, wantCalls: 2,
		},
		{
			name: "response read",
			transport: func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &providerTestBody{reader: failingReader{err: readCause}}, Request: request}, nil
			},
			cause: readCause, wantCalls: 1,
		},
		{
			name: "response close",
			transport: func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &providerTestBody{reader: strings.NewReader(validBody), closeErr: closeCause}, Request: request}, nil
			},
			cause: closeCause, wantCalls: 1,
		},
		{
			name: "invalid JSON response",
			transport: func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private malformed provider response")), Request: request}, nil
			},
			wantJSON: true, wantCalls: 1,
		},
		{
			name: "provider status",
			transport: func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"private provider response"}`)), Request: request}, nil
			},
			status: http.StatusServiceUnavailable, wantCalls: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			provider, err := NewCloudflareDNSProvider(CloudflareDNSConfig{
				BaseURL: "http://127.0.0.1/client/v4/", ZoneID: "zone_123", TokenReference: "secret/cloudflare",
				TokenSource: testSecretSource{value: []byte("token")},
				HTTPClient: &http.Client{Transport: providerRoundTripper(func(request *http.Request) (*http.Response, error) {
					calls++
					return test.transport(request)
				})},
				MaxAttempts: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Present(context.Background(), DNS01Record{Domain: "preview.example.test", Name: "_acme-challenge.preview.example.test", Value: "value", TTL: time.Minute})
			if err == nil || calls != test.wantCalls {
				t.Fatalf("Present calls=%d err=%v", calls, err)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("error does not retain original cause: %v", err)
			}
			if test.wantJSON {
				var syntaxError *json.SyntaxError
				if !errors.As(err, &syntaxError) {
					t.Fatalf("invalid JSON cause was not retained: %v", err)
				}
			}
			if test.status != 0 {
				var diagnosticStatus interface{ DiagnosticStatus() int }
				if !errors.As(err, &diagnosticStatus) || diagnosticStatus.DiagnosticStatus() != test.status {
					t.Fatalf("provider status diagnostic = %v, want %d", diagnosticStatus, test.status)
				}
			}
			for _, secret := range []string{privateCauseText, readCause.Error(), closeCause.Error(), "private provider response", "private malformed provider response"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error exposed private detail %q: %v", secret, err)
				}
			}
		})
	}
}

func TestCloudflareDNSProviderWaitPreservesFinalLookupCause(t *testing.T) {
	lookupCause := errors.New("private DNS resolver diagnostic")
	provider, err := NewCloudflareDNSProvider(CloudflareDNSConfig{
		BaseURL: "http://127.0.0.1/client/v4/", ZoneID: "zone_123", TokenReference: "secret/cloudflare",
		TokenSource: testSecretSource{value: []byte("token")}, Resolver: failingTXTResolver{err: lookupCause},
		PropagationWait: 5 * time.Millisecond, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = provider.Wait(context.Background(), DNS01Record{Domain: "preview.example.test", Name: "_acme-challenge.preview.example.test", Value: "value", TTL: time.Minute, ProviderID: "record_123"})
	if err == nil || !errors.Is(err, ErrDNSChallengePending) || !errors.Is(err, lookupCause) {
		t.Fatalf("Wait error=%v, want pending plus the final resolver cause", err)
	}
	if strings.Contains(err.Error(), lookupCause.Error()) {
		t.Fatalf("Wait exposed resolver detail: %v", err)
	}

	provider, err = NewCloudflareDNSProvider(CloudflareDNSConfig{
		BaseURL: "http://127.0.0.1/client/v4/", ZoneID: "zone_123", TokenReference: "secret/cloudflare",
		TokenSource: testSecretSource{value: []byte("token")}, Resolver: failingTXTResolver{err: &net.DNSError{
			Err: "private resolver response", Name: "_acme-challenge.preview.example.test", IsNotFound: true, UnwrapErr: syscall.EIO,
		}},
		PropagationWait: 5 * time.Millisecond, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = provider.Wait(context.Background(), DNS01Record{Domain: "preview.example.test", Name: "_acme-challenge.preview.example.test", Value: "value", TTL: time.Minute, ProviderID: "record_123"})
	if err == nil || !errors.Is(err, ErrDNSChallengePending) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("Wait error=%v, want pending plus independent NXDOMAIN cause", err)
	}
	if strings.Contains(err.Error(), "private resolver response") {
		t.Fatalf("Wait exposed private NXDOMAIN detail: %v", err)
	}
}

func TestCloudflareDNSProviderWaitKeepsOrdinaryPropagationPendingExpected(t *testing.T) {
	tests := []struct {
		name     string
		resolver TXTResolver
	}{
		{name: "no TXT answer", resolver: &testTXTResolver{}},
		{name: "pure NXDOMAIN", resolver: failingTXTResolver{err: &net.DNSError{Err: "no such host", Name: "_acme-challenge.preview.example.test", IsNotFound: true}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := NewCloudflareDNSProvider(CloudflareDNSConfig{
				BaseURL: "http://127.0.0.1/client/v4/", ZoneID: "zone_123", TokenReference: "secret/cloudflare",
				TokenSource: testSecretSource{value: []byte("token")}, Resolver: test.resolver,
				PropagationWait: 5 * time.Millisecond, PollInterval: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			err = provider.Wait(context.Background(), DNS01Record{Domain: "preview.example.test", Name: "_acme-challenge.preview.example.test", Value: "value", TTL: time.Minute, ProviderID: "record_123"})
			if err != ErrDNSChallengePending {
				t.Fatalf("Wait error=%v, want direct pending deferral", err)
			}
		})
	}
}

func TestCloudflareDNSProviderWaitPreservesParentCancellationCause(t *testing.T) {
	parentCause := errors.New("private parent cancellation diagnostic")
	resolver := waitingTXTResolver{started: make(chan struct{}, 1)}
	provider, err := NewCloudflareDNSProvider(CloudflareDNSConfig{
		BaseURL: "http://127.0.0.1/client/v4/", ZoneID: "zone_123", TokenReference: "secret/cloudflare",
		TokenSource: testSecretSource{value: []byte("token")}, Resolver: resolver,
		PropagationWait: time.Second, PollInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- provider.Wait(ctx, DNS01Record{Domain: "preview.example.test", Name: "_acme-challenge.preview.example.test", Value: "value", TTL: time.Minute, ProviderID: "record_123"})
	}()
	<-resolver.started
	cancel(parentCause)
	result := <-done
	if !errors.Is(result, context.Canceled) || !errors.Is(result, parentCause) {
		t.Fatalf("Wait error=%v, want parent cancellation and its cause", result)
	}
	if strings.Contains(result.Error(), parentCause.Error()) {
		t.Fatalf("Wait exposed parent cancellation detail: %v", result)
	}
}

func TestDNSCAAInspectorUsesClosestPolicyAndWildcardTag(t *testing.T) {
	queryer := &testCAAQueryer{answers: map[string][]*dns.CAA{
		"_example.test.": {{Hdr: dns.RR_Header{Name: "_example.test.", Rrtype: dns.TypeCAA, Class: dns.ClassINET}, Tag: "issue", Value: "other-ca.example"}},
		"example.test.":  {{Hdr: dns.RR_Header{Name: "example.test.", Rrtype: dns.TypeCAA, Class: dns.ClassINET}, Tag: "issue", Value: "letsencrypt.org"}, {Hdr: dns.RR_Header{Name: "example.test.", Rrtype: dns.TypeCAA, Class: dns.ClassINET}, Tag: "issuewild", Value: "other-ca.example"}},
	}}
	inspector, err := NewDNSCAAInspector(DNSCAAInspectorConfig{Server: "127.0.0.1:53", Queryer: queryer, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := inspector.Check(context.Background(), "www.example.test", "letsencrypt.org")
	if err != nil || ordinary.State != "ready" {
		t.Fatalf("ordinary=%+v err=%v", ordinary, err)
	}
	wildcard, err := inspector.Check(context.Background(), "*.example.test", "letsencrypt.org")
	// The policy result is intentionally a typed state rather than a Go
	// error, so callers can expose a stable CAA diagnostic.
	if err != nil || wildcard.State != "blocked" {
		t.Fatalf("wildcard=%+v err=%v", wildcard, err)
	}
}

func TestDNSCAAInspectorAllowsUnrestrictedAndHonorsCriticalPolicies(t *testing.T) {
	tests := []struct {
		name       string
		records    []*dns.CAA
		hostname   string
		wantState  string
		wantReason string
	}{
		{name: "iodef only ordinary", records: []*dns.CAA{{Tag: "iodef", Value: "mailto:security@example.test"}}, hostname: "www.example.test", wantState: "ready"},
		{name: "iodef only wildcard", records: []*dns.CAA{{Tag: "iodef", Value: "mailto:security@example.test"}}, hostname: "*.example.test", wantState: "ready"},
		{name: "wildcard falls back to issue", records: []*dns.CAA{{Tag: "issue", Value: "letsencrypt.org"}}, hostname: "*.example.test", wantState: "ready"},
		{name: "wildcard issuewild wins", records: []*dns.CAA{{Tag: "issue", Value: "letsencrypt.org"}, {Tag: "issuewild", Value: "other-ca.example"}}, hostname: "*.example.test", wantState: "blocked", wantReason: "caa_issuer_not_authorized"},
		{name: "empty issue denies", records: []*dns.CAA{{Tag: "issue", Value: "; accounturi=https://example.test"}}, hostname: "www.example.test", wantState: "blocked", wantReason: "caa_issuance_forbidden"},
		{name: "unknown noncritical ignored", records: []*dns.CAA{{Flag: 0, Tag: "accounturi", Value: "opaque"}}, hostname: "www.example.test", wantState: "ready"},
		{name: "unknown critical blocks", records: []*dns.CAA{{Flag: 128, Tag: "accounturi", Value: "opaque"}}, hostname: "www.example.test", wantState: "blocked", wantReason: "caa_unsupported_critical_tag"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queryer := &testCAAQueryer{answers: map[string][]*dns.CAA{"example.test.": test.records}}
			inspector, err := NewDNSCAAInspector(DNSCAAInspectorConfig{Server: "127.0.0.1:53", Queryer: queryer, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			result, err := inspector.Check(context.Background(), test.hostname, "letsencrypt.org")
			if err != nil || result.State != test.wantState || result.FailureCode != test.wantReason {
				t.Fatalf("result=%+v err=%v, want state=%s reason=%s", result, err, test.wantState, test.wantReason)
			}
		})
	}
}

func TestDNSCAAInspectorRetainsResolverCauseWithoutExposingItsText(t *testing.T) {
	cause := errors.New("private resolver endpoint detail")
	inspector, err := NewDNSCAAInspector(DNSCAAInspectorConfig{Server: "127.0.0.1:53", Queryer: &testCAAQueryer{err: cause}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inspector.Check(context.Background(), "app.example.test", "letsencrypt.org")
	if err == nil || !errors.Is(err, ErrCAAUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("CAA lookup error=%v, want unavailable plus original resolver cause", err)
	}
	if strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("CAA lookup exposed resolver details: %v", err)
	}
}

type testCAAQueryer struct {
	answers map[string][]*dns.CAA
	err     error
}

func (q *testCAAQueryer) ExchangeContext(_ context.Context, request *dns.Msg, _ string) (*dns.Msg, time.Duration, error) {
	if q.err != nil {
		return nil, 0, q.err
	}
	name := request.Question[0].Name
	answers, ok := q.answers[name]
	if !ok {
		return &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeNameError}}, 0, nil
	}
	message := new(dns.Msg)
	message.SetReply(request)
	for _, answer := range answers {
		message.Answer = append(message.Answer, answer)
	}
	return message, 0, nil
}

type providerRoundTripper func(*http.Request) (*http.Response, error)

func (f providerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type providerTestBody struct {
	reader   io.Reader
	closeErr error
}

func (b *providerTestBody) Read(data []byte) (int, error) { return b.reader.Read(data) }
func (b *providerTestBody) Close() error                  { return b.closeErr }

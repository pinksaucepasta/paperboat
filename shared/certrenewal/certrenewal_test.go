package certrenewal

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

const day = 24 * time.Hour

// The lifetimes Let's Encrypt issues: today, from 2027-02-10, from 2028-02-16, and shortlived.
var lifetimes = map[string]time.Duration{"90d": 90 * day, "64d": 64 * day, "45d": 45 * day, "160h": 160 * time.Hour}

var testKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

func leafAt(t *testing.T, serial int64, from time.Time, lifetime time.Duration) *x509.Certificate {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: from, NotAfter: from.Add(lifetime), DNSNames: []string{"a.example.test"}, AuthorityKeyId: []byte{1, 2, 3}}
	der, err := x509.CreateCertificate(rand.Reader, template, &x509.Certificate{SubjectKeyId: []byte{1, 2, 3}}, &testKey.PublicKey, testKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// ca answers the directory, renewal information and orders.
type ca struct {
	*httptest.Server
	window            func() (time.Time, time.Time) // nil: renewal information answers 503
	ari, orders       int
	replaces, plain   int
	refuseReplacement bool
}

func newCA(t *testing.T, ari bool) *ca {
	c := &ca{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", fmt.Sprintf("n%d", time.Now().UnixNano()))
		reply := func(code int, v any) {
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.URL.Path == "/directory":
			d := map[string]any{"newNonce": c.URL + "/nonce", "newAccount": c.URL + "/acct", "newOrder": c.URL + "/order"}
			if ari {
				d["renewalInfo"] = c.URL + "/ari"
			}
			reply(200, d)
		case strings.HasPrefix(r.URL.Path, "/ari/"):
			c.ari++
			if c.window == nil {
				reply(400, map[string]any{})
				return
			}
			start, end := c.window()
			w.Header().Set("Retry-After", "21600")
			reply(200, map[string]any{"suggestedWindow": map[string]any{"start": start, "end": end}})
		case r.URL.Path == "/order":
			var jws struct{ Payload string }
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &jws)
			payload, _ := base64.RawURLEncoding.DecodeString(jws.Payload)
			var order struct{ Replaces string }
			_ = json.Unmarshal(payload, &order)
			if order.Replaces != "" {
				c.replaces++
				if c.refuseReplacement {
					w.Header().Set("Content-Type", "application/problem+json")
					reply(409, map[string]any{"type": "urn:ietf:params:acme:error:alreadyReplaced"})
					return
				}
			} else {
				c.plain++
			}
			w.Header().Set("Location", c.URL+"/order/1")
			reply(201, map[string]any{"status": "pending", "authorizations": []string{c.URL + "/authz"}, "finalize": c.URL + "/fin"})
		case r.URL.Path == "/order/1":
			reply(200, map[string]any{"status": "pending", "authorizations": []string{c.URL + "/authz"}, "finalize": c.URL + "/fin"})
		}
	}))
	t.Cleanup(c.Close)
	return c
}

// cycles runs a fake clock over several lifetimes: the worker looks every ten minutes and every attempt of the first
// failing period after a certificate comes due fails.
func cycles(t *testing.T, plan func(*x509.Certificate, time.Time) time.Time, lifetime, failing time.Duration) {
	t.Helper()
	now := time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)
	serial, renewals := int64(1), 0
	leaf := leafAt(t, serial, now, lifetime)
	end := now.Add(4 * lifetime)
	var lastRenewal time.Time
	for ; now.Before(end); now = now.Add(10 * time.Minute) {
		if !now.Before(leaf.NotAfter) {
			t.Fatalf("certificate expired at %s", now)
		}
		at := plan(leaf, now)
		if now.Before(at) || now.Before(at.Add(failing)) {
			continue
		}
		if !lastRenewal.IsZero() && now.Sub(lastRenewal) < lifetime*45/100 {
			t.Fatalf("renewed again after %s of a %s certificate", now.Sub(lastRenewal), lifetime)
		}
		serial++
		leaf, lastRenewal = leafAt(t, serial, now, lifetime), now
		renewals++
	}
	if renewals < 4 || renewals > 8 {
		t.Fatalf("%d renewals in four lifetimes of %s", renewals, lifetime)
	}
}

func TestOneRenewalPerCycleWithoutTheCAsAdviceAndAfterFailures(t *testing.T) {
	for name, lifetime := range lifetimes {
		t.Run(name, func(t *testing.T) {
			var none *Schedule
			plan := func(leaf *x509.Certificate, now time.Time) time.Time {
				return none.Plan(t.Context(), leaf, now).RenewAt
			}
			cycles(t, plan, lifetime, 0)
			cycles(t, plan, lifetime, lifetime/4) // the first quarter of a lifetime of attempts fails
		})
	}
}

func TestOneRenewalPerCycleInTheCAsWindow(t *testing.T) {
	for name, lifetime := range lifetimes {
		t.Run(name, func(t *testing.T) {
			c := newCA(t, true)
			s := New(c.URL+"/directory", nil)
			var current *x509.Certificate
			c.window = func() (time.Time, time.Time) {
				return current.NotBefore.Add(lifetime * 60 / 100), current.NotBefore.Add(lifetime * 62 / 100)
			}
			cycles(t, func(leaf *x509.Certificate, now time.Time) time.Time {
				current = leaf
				plan := s.Plan(t.Context(), leaf, now)
				start, end := c.window()
				if !plan.FromCA || plan.RenewAt.Before(start.Add(-time.Second)) || plan.RenewAt.After(end) {
					t.Fatalf("plan %+v outside the CA's window %s..%s", plan, start, end)
				}
				return plan.RenewAt
			}, lifetime, lifetime/4)
			if most := int(4*lifetime/(6*time.Hour)) + 10; c.ari > most {
				t.Fatalf("asked the CA %d times, want at most %d (every six hours)", c.ari, most)
			}
		})
	}
}

// A revocation event moves the window into the past; an unreachable endpoint keeps the last answer; a window past the
// expiry is cut off; a CA without renewal information uses the fallback.
func TestPlanFollowsAndOutlivesTheCA(t *testing.T) {
	now := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	leaf := leafAt(t, 7, now, 64*day)
	c := newCA(t, true)
	s := New(c.URL+"/directory", nil)
	c.window = func() (time.Time, time.Time) { return now.Add(30 * day), now.Add(31 * day) }
	first := s.Plan(t.Context(), leaf, now)
	if !first.FromCA || first.RenewAt.Before(now.Add(30*day-time.Second)) || first.RenewAt.After(now.Add(31*day)) {
		t.Fatalf("plan %+v", first)
	}
	if asked := c.ari; s.Plan(t.Context(), leaf, now.Add(time.Minute)) != first || c.ari != asked {
		t.Fatal("asked again before the answer was due for a check")
	}
	c.window = nil
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second) // the client backs off between its attempts
	if plan := s.Plan(ctx, leaf, now.Add(7*time.Hour)); plan.RenewAt != first.RenewAt {
		t.Fatalf("unreachable renewal information moved the plan to %+v", plan)
	}
	cancel()
	c.window = func() (time.Time, time.Time) { return now.Add(-2 * day), now.Add(-day) }
	if plan := s.Plan(t.Context(), leaf, now.Add(9*time.Hour)); plan.RenewAt.After(now) {
		t.Fatalf("a window in the past is renewed now, got %+v", plan)
	}
	late := leafAt(t, 8, now, 64*day)
	c.window = func() (time.Time, time.Time) { return now.Add(100 * day), now.Add(101 * day) }
	if plan := s.Plan(t.Context(), late, now); plan.RenewAt.After(late.NotAfter.Add(-10 * day)) {
		t.Fatalf("a window past the expiry was followed: %+v", plan)
	}
	if plan := New(newCA(t, false).URL+"/directory", nil).Plan(t.Context(), leaf, now); plan.FromCA || !plan.RenewAt.Equal(Fallback(leaf)) {
		t.Fatalf("CA without renewal information: %+v", plan)
	}
}

func TestFallbackStaggersAFleet(t *testing.T) {
	now := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	for name, lifetime := range lifetimes {
		margin := Margin(now, now.Add(lifetime))
		buckets := map[int64]int{}
		for i := range 200 {
			at := Fallback(leafAt(t, int64(i+1), now, lifetime))
			early := now.Add(lifetime - margin).Sub(at)
			if early < 0 || early > margin/10+time.Second {
				t.Fatalf("%s: renews %s before the margin", name, early)
			}
			buckets[int64(early*10/(margin/10+time.Second))]++
		}
		for bucket, n := range buckets {
			if n > 60 {
				t.Fatalf("%s: %d of 200 certificates renew in a tenth of the spread (bucket %d)", name, n, bucket)
			}
		}
	}
}

func TestAuthorizeOrderNamesTheReplacedCertificateAndFallsBackToAPlainOrder(t *testing.T) {
	leaf := leafAt(t, 9, time.Now(), 45*day)
	for _, test := range []struct {
		name           string
		ari, refuse    bool
		replaces       *x509.Certificate
		named, unnamed int
	}{
		{"first certificate", true, false, nil, 0, 1},
		{"renewal", true, false, leaf, 1, 0},
		{"already replaced", true, true, leaf, 1, 1},
		{"CA without renewal information", false, false, leaf, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newCA(t, test.ari)
			c.refuseReplacement = test.refuse
			client := &acme.Client{Key: testKey, KID: acme.KeyID(c.URL + "/acct/1"), DirectoryURL: c.URL + "/directory"}
			order, err := AuthorizeOrder(t.Context(), client, acme.DomainIDs("a.example.test"), test.replaces, "")
			if err != nil || order.URI != c.URL+"/order/1" || len(order.AuthzURLs) != 1 || c.replaces != test.named || c.plain != test.unnamed {
				t.Fatalf("order %+v, %v; named %d plain %d", order, err, c.replaces, c.plain)
			}
		})
	}
}

func TestRetryDelayStaysUnderTheFailedValidationLimit(t *testing.T) {
	elapsed, orders := time.Duration(0), 1
	for attempt := 1; ; attempt++ {
		if elapsed += RetryDelay(attempt); elapsed >= time.Hour {
			break
		}
		orders++
	}
	if orders > 4 || RetryDelay(100) != time.Hour {
		t.Fatalf("%d orders in the first hour, then every %s", orders, RetryDelay(100))
	}
}

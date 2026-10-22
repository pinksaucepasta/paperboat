package auditstream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSignatureIsHMACOfTimestampAndBody(t *testing.T) {
	at := time.Unix(1761200000, 0)
	got := signature("pbsig_secret", at, []byte("{\"id\":1}\n"))
	m := hmac.New(sha256.New, []byte("pbsig_secret"))
	m.Write([]byte("1761200000.{\"id\":1}\n"))
	if want := "t=1761200000,v1=" + hex.EncodeToString(m.Sum(nil)); got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}

func TestDestinationsOnlyOnThePublicInternet(t *testing.T) {
	for ip, public := range map[string]bool{"8.8.8.8": true, "2606:4700::1111": true, "10.0.0.1": false, "127.0.0.1": false, "169.254.169.254": false,
		"100.95.70.63": false, "192.168.1.1": false, "::1": false, "fd7a:115c:a1e0::1": false} {
		if publicIP(net.ParseIP(ip)) != public {
			t.Errorf("%s public=%v", ip, !public)
		}
	}
	for raw, ok := range map[string]bool{"https://http-inputs.example.test/services/collector/raw": true, "http://siem.example.test": false,
		"https://10.0.0.5/x": false, "https://localhost/x": false, "https://user:pw@siem.example.test": false, "https://metadata.internal/x": false} {
		if validURL(raw) != ok {
			t.Errorf("%s accepted=%v", raw, !ok)
		}
	}
}

func TestInputValidation(t *testing.T) {
	good := Input{URL: "https://s3.example.test", Region: "eu-central-1", Bucket: "audit-logs", Prefix: "paperboat/", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret"}
	if !good.validate("s3", true) {
		t.Fatal("valid s3 refused")
	}
	if (Input{URL: "https://siem.example.test/hook", HeaderName: "Authorization", HeaderValue: "Splunk x"}).validate("https", true) != true {
		t.Fatal("valid https refused")
	}
	for name, in := range map[string]Input{
		"reserved header": {URL: "https://siem.example.test", HeaderName: "Paperboat-Signature", HeaderValue: "x"},
		"header no value": {URL: "https://siem.example.test", HeaderName: "Authorization"},
		"bad bucket":      {URL: good.URL, Region: good.Region, Bucket: "Bad_Bucket", AccessKeyID: "AKIA", SecretAccessKey: "s"},
		"no secret":       {URL: good.URL, Region: good.Region, Bucket: good.Bucket, AccessKeyID: "AKIA"},
	} {
		kind := "https"
		if in.Bucket != "" || in.AccessKeyID != "" {
			kind = "s3"
		}
		if in.validate(kind, true) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestObjectKeysAreStableAndEncodedLikeTheSignature(t *testing.T) {
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	if k := objectKey("/logs/", "team:acme", at, 10, 42); k != "logs/team-acme/2026/10/23/10-42.ndjson.gz" {
		t.Fatalf("key = %s", k)
	}
	u, err := objectURL("https://s3.example.test/", "audit", "a b/c.ndjson.gz")
	if err != nil || !strings.HasSuffix(u.String(), "/audit/a%20b/c.ndjson.gz") {
		t.Fatalf("url = %v err=%v", u, err)
	}
}

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstallationChallengeBindsNodeAndNonce(t *testing.T) {
	const secret = "runtime-credential-for-test-0123456789"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	handler := installationChallenge("node_a", "edge.example.test", secret, next)
	nonce := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("n", 32)))
	request := httptest.NewRequest("GET", "https://edge.example.test/.well-known/paperboat-installation?challenge="+nonce, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var response map[string]string
	if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
		t.Fatal("challenge failed")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("paperboat-installation/v1\nnode_a\n" + nonce))
	if response["node_id"] != "node_a" || response["proof"] != base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("proof not bound")
	}
	for _, target := range []string{"https://other.example.test/.well-known/paperboat-installation?challenge=" + nonce, "https://edge.example.test/.well-known/paperboat-installation?challenge=bad", "https://edge.example.test/.well-known/paperboat-installation?challenge=" + nonce + "&challenge=" + nonce} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest("GET", target, nil))
		if r.Code == 200 {
			t.Fatal("invalid challenge accepted")
		}
	}
}

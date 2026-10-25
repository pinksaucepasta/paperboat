package connectorprotocol

import (
	"errors"
	"strings"
	"testing"
)

func TestHTTP01ResponseValidate(t *testing.T) {
	valid := HTTP01Response{Hostname: "app.example.test", Token: "tok_A-9", KeyAuthorization: "tok_A-9.thumb"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	for name, mutate := range map[string]func(*HTTP01Response){
		"wildcard_host":      func(r *HTTP01Response) { r.Hostname = "*.example.test" },
		"uppercase_host":     func(r *HTTP01Response) { r.Hostname = "App.example.test" },
		"host_with_port":     func(r *HTTP01Response) { r.Hostname = "app.example.test:80" },
		"empty_host":         func(r *HTTP01Response) { r.Hostname = "" },
		"token_with_slash":   func(r *HTTP01Response) { r.Token = "a/b"; r.KeyAuthorization = "a/b.thumb" },
		"token_too_long":     func(r *HTTP01Response) { r.Token = strings.Repeat("a", 257); r.KeyAuthorization = r.Token + ".t" },
		"foreign_key_auth":   func(r *HTTP01Response) { r.KeyAuthorization = "other.thumb" },
		"empty_thumbprint":   func(r *HTTP01Response) { r.KeyAuthorization = "tok_A-9." },
		"oversized_key_auth": func(r *HTTP01Response) { r.KeyAuthorization = "tok_A-9." + strings.Repeat("x", 600) },
	} {
		t.Run(name, func(t *testing.T) {
			response := valid
			mutate(&response)
			if err := response.Validate(); !errors.Is(err, ErrHTTP01ResponseInvalid) {
				t.Fatalf("invalid response accepted: %v", err)
			}
		})
	}
}

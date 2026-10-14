// Package nodeauth binds service credentials to an exact installation node.
package nodeauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

var ErrNotFound = errors.New("node credential not found")

type Resolver interface {
	RuntimeNode(context.Context, string) (string, error)
	RuntimeCredential(context.Context, string) (string, error)
	IsSelfHosted(context.Context, string) (bool, error)
}

func Bearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return ""
	}
	value := strings.TrimPrefix(values[0], "Bearer ")
	if value == "" || strings.TrimSpace(value) != value {
		return ""
	}
	return value
}

func Managed(r *http.Request, credential string) bool {
	return credential != "" && subtle.ConstantTimeCompare([]byte(Bearer(r)), []byte(credential)) == 1
}

func Matches(ctx context.Context, resolver Resolver, r *http.Request, managedCredential, nodeID string) (bool, error) {
	if nodeID == "" {
		return false, nil
	}
	if Managed(r, managedCredential) {
		return true, nil
	}
	if resolver == nil || Bearer(r) == "" {
		return false, nil
	}
	node, err := resolver.RuntimeNode(ctx, Bearer(r))
	if err != nil {
		if pureNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return node == nodeID, nil
}

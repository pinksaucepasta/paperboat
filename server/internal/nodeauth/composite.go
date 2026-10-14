package nodeauth

import (
	"context"
	"reflect"
)

type Composite []Resolver

func (c Composite) RuntimeNode(ctx context.Context, credential string) (string, error) {
	for _, resolver := range c {
		if resolver == nil {
			continue
		}
		node, err := resolver.RuntimeNode(ctx, credential)
		if err == nil && node != "" {
			return node, nil
		}
		if err != nil {
			if pureNotFound(err) {
				continue
			}
			return "", err
		}
	}
	return "", ErrNotFound
}
func (c Composite) RuntimeCredential(ctx context.Context, node string) (string, error) {
	for _, resolver := range c {
		if resolver == nil {
			continue
		}
		value, err := resolver.RuntimeCredential(ctx, node)
		if err == nil && value != "" {
			return value, nil
		}
		if err != nil {
			if pureNotFound(err) {
				continue
			}
			return "", err
		}
	}
	return "", ErrNotFound
}

// pureNotFound treats a resolver miss as fallback only when the complete,
// bounded error tree consists of the exact miss sentinel. A database or other
// operational error joined with ErrNotFound must not be hidden by another
// resolver in the chain.
func pureNotFound(err error) (notFound bool) {
	defer func() {
		if recover() != nil {
			notFound = false
		}
	}()
	if err == nil {
		return false
	}
	nodes, matched := 0, false
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		if current == nil || depth >= 8 || nodes >= 16 {
			return false
		}
		typ := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		if typ == nil || (value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Func) && value.IsNil() {
			return false
		}
		nodes++
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			return child != nil && visit(child, depth+1)
		}
		if !typ.Comparable() || typ != reflect.TypeOf(ErrNotFound) || current != ErrNotFound {
			return false
		}
		matched = true
		return true
	}
	return visit(err, 0) && matched
}

// IsNotFound reports a resolver miss only when every leaf in the bounded
// error tree is the exact ErrNotFound sentinel.
func IsNotFound(err error) bool { return pureNotFound(err) }
func (c Composite) IsSelfHosted(ctx context.Context, node string) (bool, error) {
	for _, resolver := range c {
		if resolver == nil {
			continue
		}
		yes, err := resolver.IsSelfHosted(ctx, node)
		if err != nil {
			return false, err
		}
		if yes {
			return true, nil
		}
	}
	return false, nil
}

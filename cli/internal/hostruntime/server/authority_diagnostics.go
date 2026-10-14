package server

import (
	"context"
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
)

func expectedCredentialRejection(err error) bool {
	if err == nil {
		return false
	}
	type node struct {
		err  error
		path map[error]struct{}
	}
	stack := []node{{err: err}}
	visited := 0
	for len(stack) > 0 {
		if visited >= 16 {
			return false
		}
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current.err == nil {
			return false
		}
		value := reflect.ValueOf(current.err)
		if nilable(value.Kind()) && value.IsNil() {
			return false
		}
		visited++
		path := current.path
		typeOf := reflect.TypeOf(current.err)
		if typeOf.Comparable() {
			if _, cycle := path[current.err]; cycle {
				return false
			}
			next := make(map[error]struct{}, len(path)+1)
			for previous := range path {
				next[previous] = struct{}{}
			}
			next[current.err] = struct{}{}
			path = next
		}
		if current.err == ErrCredentialPolicy {
			continue
		}
		if credentialError, ok := current.err.(*auth.Error); ok {
			if credentialError.Code == auth.KeyUnknown && credentialError.Cause != nil {
				return false
			}
			continue
		}
		switch wrapped := current.err.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(stack) > 16-visited {
				return false
			}
			for index := len(children) - 1; index >= 0; index-- {
				stack = append(stack, node{err: children[index], path: path})
			}
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(stack)+1 > 16-visited {
				return false
			}
			stack = append(stack, node{err: child, path: path})
		default:
			return false
		}
	}
	return visited > 0
}

func reportAuthorizationFailure(ctx context.Context, err error) bool {
	if err == nil || expectedCredentialRejection(err) || allErrorLeavesMatch(err, func(leaf error) bool { return leaf == context.Canceled }) {
		return false
	}
	if allErrorLeavesMatch(err, func(leaf error) bool { return leaf == context.DeadlineExceeded }) {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "auth", "peer_authority", "peer_authority_failed", err)
		return true
	}
	errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "auth", "peer_authority", "peer_authority_failed", err)
	return true
}

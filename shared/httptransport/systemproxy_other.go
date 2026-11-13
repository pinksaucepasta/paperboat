//go:build !darwin && !windows

package httptransport

import "context"

type NativeSystemProxySource struct{}

func (NativeSystemProxySource) Snapshot(context.Context) (ProxySnapshot, error) {
	return ProxySnapshot{}, nil
}

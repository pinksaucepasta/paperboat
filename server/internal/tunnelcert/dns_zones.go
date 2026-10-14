package tunnelcert

import (
	"context"
	"strings"
)

// DNS01Zones routes every lifecycle operation to its explicitly configured
// authoritative suffix. Unknown names are refused before any provider request.
type DNS01Zones struct{ providers map[string]DNS01Provider }

func NewDNS01Zones(providers map[string]DNS01Provider) (*DNS01Zones, error) {
	if len(providers) == 0 || len(providers) > 8 {
		return nil, ErrInvalid
	}
	zones := &DNS01Zones{providers: make(map[string]DNS01Provider, len(providers))}
	for suffix, provider := range providers {
		name, wildcard, err := normalizeHostname(suffix)
		if err != nil || wildcard || provider == nil {
			return nil, ErrInvalid
		}
		if _, exists := zones.providers[name]; exists {
			return nil, ErrInvalid
		}
		zones.providers[name] = provider
	}
	return zones, nil
}

func (z *DNS01Zones) provider(record DNS01Record) (DNS01Provider, error) {
	if z == nil || !validDNS01Record(record) {
		return nil, ErrDNSChallengeUnavailable
	}
	name := strings.ToLower(strings.TrimSuffix(record.Name, "."))
	var selected DNS01Provider
	longest := 0
	for suffix, provider := range z.providers {
		if (name == suffix || strings.HasSuffix(name, "."+suffix)) && len(suffix) > longest {
			selected, longest = provider, len(suffix)
		}
	}
	if selected == nil {
		return nil, ErrDNSChallengeUnavailable
	}
	return selected, nil
}

func (z *DNS01Zones) Present(ctx context.Context, record DNS01Record) (DNS01Record, error) {
	provider, err := z.provider(record)
	if err != nil {
		return DNS01Record{}, err
	}
	return provider.Present(ctx, record)
}

func (z *DNS01Zones) Wait(ctx context.Context, record DNS01Record) error {
	provider, err := z.provider(record)
	if err != nil {
		return err
	}
	return provider.Wait(ctx, record)
}

func (z *DNS01Zones) Cleanup(ctx context.Context, record DNS01Record) error {
	provider, err := z.provider(record)
	if err != nil {
		return err
	}
	return provider.Cleanup(ctx, record)
}

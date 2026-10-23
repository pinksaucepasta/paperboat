package usermachines

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/runtimeattachment"
	"strings"
)

func (s *Service) runtimeIngressURL(ctx context.Context, environmentID, publicHost string) (string, error) {
	value, err := runtimeattachment.IngressURL(ctx, s.db, environmentID, publicHost)
	if errors.Is(err, runtimeattachment.ErrUnavailable) {
		return "", ErrProvisioningUnavailable
	}
	return value, err
}

func runtimeWebsocketBase(httpBase string) string {
	return strings.Replace(httpBase, "https://", "wss://", 1)
}

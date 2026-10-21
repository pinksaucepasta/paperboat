package tunnelenrollment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

const originHealthSchema = "paperboat.origin-health/v1"

type originHealthRoute struct {
	RouteID    string    `json:"route_id"`
	State      string    `json:"state"`
	Code       string    `json:"code,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

type originHealthRequest struct {
	Schema            string              `json:"schema"`
	SessionID         string              `json:"session_id"`
	ProcessGeneration uint64              `json:"process_generation"`
	ConfigGeneration  uint64              `json:"config_generation"`
	ConfigContentHash string              `json:"content_hash"`
	Routes            []originHealthRoute `json:"routes"`
}

// ReportOriginHealth tells the control plane whether each route's local app is reachable, bound to
// the connector session of the report. It authenticates like carrier bootstrap: the renewable
// machine identity and a proof over the exact method, path and body.
func (s *HTTPSProductionAssemblySource) ReportOriginHealth(ctx context.Context, report tunnelmanager.OriginHealthReport) error {
	if s == nil || ctx == nil || len(report.Routes) == 0 {
		return ErrInvalid
	}
	routes := make([]originHealthRoute, 0, len(report.Routes))
	for _, route := range report.Routes {
		routes = append(routes, originHealthRoute{RouteID: route.RouteID, State: route.State, Code: route.Code, ObservedAt: route.ObservedAt.UTC()})
	}
	body, err := json.Marshal(originHealthRequest{
		Schema: originHealthSchema, SessionID: report.SessionID, ProcessGeneration: report.ProcessGeneration,
		ConfigGeneration: report.ConfigGeneration, ConfigContentHash: report.ContentHash, Routes: routes,
	})
	if err != nil {
		return err
	}
	operationID, err := randomID("operation")
	if err != nil {
		return err
	}
	token, err := s.auth.Token(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return ErrAuthentication
	}
	endpoint := *s.base
	endpoint.Path = strings.TrimRight(s.base.Path, "/") + "/v1/tunnels/" + url.PathEscape(report.TunnelID) + "/connectors/" + url.PathEscape(report.ConnectorID) + "/origin-health"
	endpoint.RawPath = ""
	proof, err := s.auth.Proof(ctx, operationID, http.MethodPost, endpoint.Path, body)
	if err != nil || len(proof) == 0 {
		return ErrAuthentication
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("X-Paperboat-Machine-Identity", token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Idempotency-Key", operationID)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.http.Do(request)
	if err != nil {
		return safeEnrollmentFailure("origin health report is unavailable", ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: origin health report refused with status %d", ErrUnavailable, response.StatusCode)
	}
	return nil
}

var _ tunnelmanager.OriginHealthReporter = (*HTTPSProductionAssemblySource)(nil)

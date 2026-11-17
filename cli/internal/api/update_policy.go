package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/cli/internal/selfupdate"
	"github.com/pinksaucepasta/paperboat/shared/clientupdate"
)

type UpdatePolicy = clientupdate.Policy

func ValidateUpdatePolicy(p UpdatePolicy) error {
	if p.Schema != clientupdate.Schema || p.Revision < 0 || len(p.Reason) > 256 || !utf8.ValidString(p.Reason) {
		return errors.New("invalid update policy")
	}
	for _, c := range p.Reason {
		if unicode.IsControl(c) {
			return errors.New("invalid update policy")
		}
	}
	if p.MinimumVersion != "" {
		if _, err := selfupdate.CompareVersions(p.MinimumVersion, p.MinimumVersion); err != nil {
			return errors.New("invalid update policy")
		}
	}
	return nil
}

func UpdatePolicyBehind(p UpdatePolicy, version string) bool {
	if ValidateUpdatePolicy(p) != nil || p.MinimumVersion == "" {
		return false
	}
	comparison, err := selfupdate.CompareVersions(version, p.MinimumVersion)
	return err != nil || comparison < 0
}

func UpdatePolicyRequired(p UpdatePolicy, version string, now time.Time) bool {
	return UpdatePolicyBehind(p, version) && (p.EnforceAt == nil || !now.Before(*p.EnforceAt))
}

func FetchUpdatePolicy(ctx context.Context, baseURL string, hc *http.Client) (UpdatePolicy, error) {
	if hc == nil {
		hc = defaultHTTPClient()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/client/update-policy", nil)
	if err != nil {
		return UpdatePolicy{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := hc.Do(request)
	if err != nil {
		return UpdatePolicy{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return UpdatePolicy{}, errors.New("update policy unavailable")
	}
	var envelope struct {
		Data UpdatePolicy `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4097))
	var extra any
	if decoder.Decode(&envelope) != nil || decoder.Decode(&extra) != io.EOF || ValidateUpdatePolicy(envelope.Data) != nil {
		return UpdatePolicy{}, errors.New("invalid update policy response")
	}
	return envelope.Data, nil
}

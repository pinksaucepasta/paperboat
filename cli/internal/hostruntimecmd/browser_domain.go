package hostruntimecmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"io"
	"strings"
)

type BrowserDomainRequest struct {
	Owner  string `json:"owner"`
	Domain string `json:"domain"`
}

func decodeBrowserDomain(r io.Reader) (BrowserDomainRequest, error) {
	var request BrowserDomainRequest
	body, err := io.ReadAll(io.LimitReader(r, 4097))
	if err != nil {
		return request, err
	}
	if len(body) > 4096 {
		return request, errors.New("browser domain request exceeds 4096 bytes")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return request, errors.New("invalid browser domain request")
	}
	domain, err := splitdns.NormalizeBrowserDomain(request.Domain)
	if err != nil {
		return request, err
	}
	request.Domain = domain
	if strings.TrimSpace(request.Owner) == "" || len(request.Owner) > 184 {
		return request, errors.New("invalid browser domain owner")
	}
	return request, nil
}

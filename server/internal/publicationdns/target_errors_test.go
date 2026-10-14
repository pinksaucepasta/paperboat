package publicationdns

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestTargetParseFailureKeepsCauseBehindStaticText(t *testing.T) {
	const privateCanary = "PUBLICATION_URL_PRIVATE_CANARY"
	endpoint := "https://operator:" + privateCanary + "@bad host.example/preview"
	_, err := Target(endpoint, "")
	if err == nil {
		t.Fatal("malformed endpoint was accepted")
	}
	if err.Error() != "publication DNS target is unavailable" || strings.Contains(err.Error(), privateCanary) {
		t.Fatalf("parse error exposed endpoint text: %q", err.Error())
	}
	var parseErr *url.Error
	if !errors.As(err, &parseErr) || parseErr.URL != endpoint {
		t.Fatalf("original URL parse cause was lost: %T %v", err, err)
	}
}

package httpapi

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

func TestDecodeEnrollRequest(t *testing.T) {
	for body, ok := range map[string]bool{
		`{"address":"a","code":"c"}`: true,
		`{"address":"a","code":"c","expect_region":"lab","expect_capability":"relay"}`: true,
		`{"address":"a","code":"c","extra":1}`:                                         false,
		`{"address":"a","code":"c","expect_region":""}`:                                false,
		`{"address":"a","code":"c","expect_region":null}`:                              false,
		`{"address":"a","code":"c","expect_capability":""}`:                            false,
	} {
		var in selfhost.EnrollRequest
		if err := decodeEnrollRequest([]byte(body), &in); (err == nil) != ok {
			t.Errorf("%s: %v", body, err)
		}
	}
}

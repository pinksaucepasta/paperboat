package auditstream

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// AWS's documented Signature Version 4 example "PUT Object" (Amazon S3 API reference,
// sig-v4-header-based-auth): the same inputs must give the same signature.
func TestSignV4MatchesTheAWSPutObjectExample(t *testing.T) {
	body := "Welcome to Amazon S3."
	req, _ := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test$file.text", strings.NewReader(body))
	req.Host = "examplebucket.s3.amazonaws.com"
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	signV4(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", hexSHA([]byte(body)), time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class, Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

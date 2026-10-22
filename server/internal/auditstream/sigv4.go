package auditstream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// signV4 signs an S3 request with AWS Signature Version 4 (header form), the scheme every
// S3-compatible store accepts: AWS S3, Cloudflare R2, Backblaze B2, MinIO, GCS interop.
// payloadHash is the hex SHA-256 of the body; the request must carry Host already.
func signV4(req *http.Request, accessKey, secretKey, region string, payloadHash string, now time.Time) {
	stamp, day := now.UTC().Format("20060102T150405Z"), now.UTC().Format("20060102")
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	names := make([]string, 0, len(req.Header)+1)
	values := map[string]string{"host": req.Host}
	names = append(names, "host")
	for name, v := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "user-agent" || lower == "content-length" {
			continue
		}
		names = append(names, lower)
		values[lower] = strings.Join(trimAll(v), ",")
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, n := range names {
		canonicalHeaders.WriteString(n + ":" + values[n] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{req.Method, canonicalPath(req.URL.Path), canonicalQuery(req.URL.Query()), canonicalHeaders.String(), signed, payloadHash}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hexSHA([]byte(canonical))
	key := hmacSHA([]byte("AWS4"+secretKey), day)
	for _, part := range []string{region, "s3", "aws4_request"} {
		key = hmacSHA(key, part)
	}
	signature := hex.EncodeToString(hmacSHA(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
}

func trimAll(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.Join(strings.Fields(s), " ")
	}
	return out
}

// canonicalPath URI-encodes every byte but the unreserved ones and '/', as AWS requires (S3 does
// not encode twice). Requests must be sent with the same encoding (see objectURL).
func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '/' || c == '-' || c == '_' || c == '.' || c == '~' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func awsEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func hexSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

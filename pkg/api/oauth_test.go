package api

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"testing"
)

func TestRFC3986EscapeSpaceIsPercent20(t *testing.T) {
	// url.QueryEscape would produce "a+b" here — RFC3986 (and OAuth1.0a,
	// RFC5849 §3.6) requires "%20".
	if got, want := rfc3986Escape("a b"), "a%20b"; got != want {
		t.Fatalf("rfc3986Escape(%q) = %q, want %q", "a b", got, want)
	}
}

func TestRFC3986EscapeUnreservedUntouched(t *testing.T) {
	s := "abcXYZ019-._~"
	if got := rfc3986Escape(s); got != s {
		t.Fatalf("rfc3986Escape(%q) = %q, want it unchanged", s, got)
	}
}

func TestRFC3986EscapeReservedChars(t *testing.T) {
	if got, want := rfc3986Escape("a=b&c/d"), "a%3Db%26c%2Fd"; got != want {
		t.Fatalf("rfc3986Escape = %q, want %q", got, want)
	}
}

// TestBuildSignatureEncodesSpaceAsPercent20 pins buildSignature's base
// string construction against a hand-verified expected value, so a
// regression back to url.QueryEscape (which would encode the space in the
// "a b" param value as '+' rather than "%20") is caught.
func TestBuildSignatureEncodesSpaceAsPercent20(t *testing.T) {
	params := map[string]string{"foo": "a b"}
	got := buildSignature("https://example.com/api", params, "consumersecret", "tokensecret")

	// baseURL "https://example.com/api" -> "https%3A%2F%2Fexample.com%2Fapi"
	// paramStr "foo=a%20b" -> escaped again for the base string, so its own
	// '%' becomes %25: "foo%3Da%2520b"
	wantBaseStr := "GET&https%3A%2F%2Fexample.com%2Fapi&foo%3Da%2520b"
	mac := hmac.New(sha1.New, []byte("consumersecret&tokensecret"))
	mac.Write([]byte(wantBaseStr))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Fatalf("buildSignature = %q, want %q (expected base string %q)", got, want, wantBaseStr)
	}
}

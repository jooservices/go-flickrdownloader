package api

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

// TestSignedGetReturnsHTTPStatusErrorOnNon200 is a regression test for the
// gap where a real (transport-level) non-200 response from Flickr's REST
// endpoint produced a plain, unclassified error — invisible to the retry
// loop's rate-limit detection, which only ever inspected the JSON body of an
// HTTP-200 response. SignedGet must now return a typed *httpStatusError so
// the caller can tell a transient status (429/5xx) from a fatal one.
func TestSignedGetReturnsHTTPStatusErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("Too Many Requests"))
	}))
	defer srv.Close()

	_, err := SignedGet("key", "secret", "token", "token-secret", srv.URL, nil)
	if err == nil {
		t.Fatal("expected an error for a 429 response")
	}
	var hse *httpStatusError
	if !errors.As(err, &hse) {
		t.Fatalf("expected *httpStatusError, got %T: %v", err, err)
	}
	if hse.status != http.StatusTooManyRequests {
		t.Fatalf("hse.status = %d, want %d", hse.status, http.StatusTooManyRequests)
	}
	if hse.hasRetryAfter {
		t.Fatalf("hasRetryAfter = true with no Retry-After header sent")
	}
}

// TestSignedGetParsesRetryAfterHeader checks the Retry-After header on a
// non-200 response is parsed into the returned error.
func TestSignedGetParsesRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("try later"))
	}))
	defer srv.Close()

	_, err := SignedGet("key", "secret", "token", "token-secret", srv.URL, nil)
	var hse *httpStatusError
	if !errors.As(err, &hse) {
		t.Fatalf("expected *httpStatusError, got %T: %v", err, err)
	}
	if !hse.hasRetryAfter || hse.retryAfter != 7*time.Second {
		t.Fatalf("retryAfter = %v (hasRetryAfter=%v), want 7s", hse.retryAfter, hse.hasRetryAfter)
	}
}

// TestSignedGetSetsUserAgent guards against reverting to the Go default
// User-Agent, which some edges/WAFs throttle more aggressively than a named
// client.
func TestSignedGetSetsUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stat":"ok"}`))
	}))
	defer srv.Close()

	if _, err := SignedGet("key", "secret", "token", "token-secret", srv.URL, nil); err != nil {
		t.Fatalf("SignedGet: %v", err)
	}
	if gotUA != userAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUA, userAgent)
	}
}

// TestGetRequestToken covers the OAuth1.0a request-token step: a
// successful, URL-encoded response is parsed into (token, secret).
func TestGetRequestToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("oauth_token=req-token&oauth_token_secret=req-secret&oauth_callback_confirmed=true"))
	}))
	defer srv.Close()

	orig := requestTokenURL
	defer func() { requestTokenURL = orig }()
	requestTokenURL = srv.URL

	token, secret, err := GetRequestToken("key", "secret")
	if err != nil {
		t.Fatalf("GetRequestToken: %v", err)
	}
	if token != "req-token" || secret != "req-secret" {
		t.Fatalf("token=%q secret=%q, want req-token/req-secret", token, secret)
	}
}

// TestGetAccessToken covers the OAuth1.0a access-token step.
func TestGetAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("oauth_token=acc-token&oauth_token_secret=acc-secret&user_nsid=123456789%40N01&username=alice"))
	}))
	defer srv.Close()

	orig := accessTokenURL
	defer func() { accessTokenURL = orig }()
	accessTokenURL = srv.URL

	token, secret, nsid, username, err := GetAccessToken("key", "secret", "req-token", "req-secret", "verifier")
	if err != nil {
		t.Fatalf("GetAccessToken: %v", err)
	}
	if token != "acc-token" || secret != "acc-secret" || nsid != "123456789@N01" || username != "alice" {
		t.Fatalf("got token=%q secret=%q nsid=%q username=%q", token, secret, nsid, username)
	}
}

func TestGetAuthorizeURL(t *testing.T) {
	got := GetAuthorizeURL("a token")
	want := authorizeURL + "?oauth_token=a+token&perms=read"
	if got != want {
		t.Fatalf("GetAuthorizeURL = %q, want %q", got, want)
	}
}

// TestOAuthGetNon200 covers OAuthGet's own status check, independent of
// SignedGet's (a genuinely different code path, both hit for real during
// `auth`).
func TestOAuthGetNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer srv.Close()

	if _, err := OAuthGet("key", "secret", "", srv.URL, nil); err == nil {
		t.Fatal("expected an error for a non-200 OAuth response")
	}
}

// TestOAuthGetProblem covers Flickr's oauth_problem convention: a 200
// response whose body still signals failure via a query-encoded field.
func TestOAuthGetProblem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("oauth_problem=permission_denied"))
	}))
	defer srv.Close()

	if _, err := OAuthGet("key", "secret", "", srv.URL, nil); err == nil {
		t.Fatal("expected an error for an oauth_problem response")
	}
}

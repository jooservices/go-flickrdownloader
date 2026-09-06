package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	requestTokenURL      = "https://www.flickr.com/services/oauth/request_token"
	authorizeURL         = "https://www.flickr.com/services/oauth/authorize"
	accessTokenURL       = "https://www.flickr.com/services/oauth/access_token"
	oauthSignatureMethod = "HMAC-SHA1"
	oauthVersion         = "1.0"
	// userAgent identifies this client to Flickr; some edges/WAFs throttle
	// the default Go-http-client/1.1 UA more aggressively than a named one.
	userAgent = "flickrdownloader"
)

func hmacSHA1(key, data string) string {
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(data))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func nonce() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// rfc3986Escape percent-encodes s per RFC3986 §2.1, as OAuth1.0a (RFC5849
// §3.6) requires for the signature base string and signed request
// parameters. url.QueryEscape is not a substitute: it encodes a space as
// '+' rather than '%20', so a strict RFC3986 verifier — which decodes the
// query string (where '+' means space) and then re-encodes per RFC3986 to
// check the signature — computes a different base string than the one used
// to sign, and rejects an otherwise-valid request.
func rfc3986Escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isRFC3986Unreserved(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isRFC3986Unreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-' || c == '.' || c == '_' || c == '~':
		return true
	}
	return false
}

func buildSignature(baseURL string, params map[string]string, consumerSecret, tokenSecret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var encodedPairs []string
	for _, k := range keys {
		encodedPairs = append(encodedPairs, rfc3986Escape(k)+"="+rfc3986Escape(params[k]))
	}
	paramStr := strings.Join(encodedPairs, "&")

	baseStr := "GET&" + rfc3986Escape(baseURL) + "&" + rfc3986Escape(paramStr)
	return hmacSHA1(consumerSecret+"&"+tokenSecret, baseStr)
}

func OAuthGet(apiKey, consumerSecret, tokenSecret, uri string, extra map[string]string) (map[string]string, error) {
	params := map[string]string{
		"oauth_nonce":            nonce(),
		"oauth_timestamp":        strconv.FormatInt(time.Now().Unix(), 10),
		"oauth_consumer_key":     apiKey,
		"oauth_signature_method": oauthSignatureMethod,
		"oauth_version":          oauthVersion,
	}
	for k, v := range extra {
		params[k] = v
	}

	sig := buildSignature(uri, params, consumerSecret, tokenSecret)
	params["oauth_signature"] = sig

	var encodedPairs []string
	for k, v := range params {
		encodedPairs = append(encodedPairs, k+"="+rfc3986Escape(v))
	}

	req, err := http.NewRequest("GET", uri+"?"+strings.Join(encodedPairs, "&"), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth failed: %s — %s", resp.Status, string(body))
	}

	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse oauth response: %w", err)
	}

	result := make(map[string]string)
	for k, v := range vals {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}

	if result["oauth_problem"] != "" {
		return nil, fmt.Errorf("oauth problem: %s", result["oauth_problem"])
	}

	return result, nil
}

func GetRequestToken(apiKey, apiSecret string) (string, string, error) {
	result, err := OAuthGet(apiKey, apiSecret, "", requestTokenURL, map[string]string{
		"oauth_callback": "oob",
	})
	if err != nil {
		return "", "", err
	}
	return result["oauth_token"], result["oauth_token_secret"], nil
}

func GetAuthorizeURL(oauthToken string) string {
	return authorizeURL + "?oauth_token=" + url.QueryEscape(oauthToken) + "&perms=read"
}

func GetAccessToken(apiKey, apiSecret, oauthToken, oauthTokenSecret, verifier string) (string, string, string, string, error) {
	result, err := OAuthGet(apiKey, apiSecret, oauthTokenSecret, accessTokenURL, map[string]string{
		"oauth_token":    oauthToken,
		"oauth_verifier": verifier,
	})
	if err != nil {
		return "", "", "", "", err
	}
	return result["oauth_token"], result["oauth_token_secret"], result["user_nsid"], result["username"], nil
}

func signParams(apiKey, apiSecret, accessToken, accessSecret, baseURL string, queryParams map[string]string) map[string]string {
	params := map[string]string{
		"oauth_nonce":            nonce(),
		"oauth_timestamp":        strconv.FormatInt(time.Now().Unix(), 10),
		"oauth_consumer_key":     apiKey,
		"oauth_token":            accessToken,
		"oauth_signature_method": oauthSignatureMethod,
		"oauth_version":          oauthVersion,
	}
	for k, v := range queryParams {
		params[k] = v
	}

	sig := buildSignature(baseURL, params, apiSecret, accessSecret)
	params["oauth_signature"] = sig

	return params
}

func SignedGet(apiKey, apiSecret, accessToken, accessSecret, uri string, queryParams map[string]string) ([]byte, error) {
	params := signParams(apiKey, apiSecret, accessToken, accessSecret, uri, queryParams)

	var encodedPairs []string
	for k, v := range params {
		encoded := k + "=" + rfc3986Escape(v)
		encodedPairs = append(encodedPairs, encoded)
	}

	reqURL := uri + "?" + strings.Join(encodedPairs, "&")
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signed get: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// A non-200 here is a transport-level failure (edge/WAF throttle,
		// transient 5xx) rather than Flickr's usual HTTP-200-with-JSON-
		// stat=fail convention isRateLimitResponse handles. Returning a
		// typed httpStatusError (instead of a plain error) lets
		// signedGetWithRetry tell a transient status worth retrying from a
		// fatal one, and honor Retry-After when the server sent one.
		hse := &httpStatusError{status: resp.StatusCode, body: body}
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			hse.retryAfter, hse.hasRetryAfter = d, true
		}
		return nil, hse
	}

	return body, nil
}

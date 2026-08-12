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

func buildSignature(baseURL string, params map[string]string, consumerSecret, tokenSecret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var encodedPairs []string
	for _, k := range keys {
		encodedPairs = append(encodedPairs, url.QueryEscape(k)+"="+url.QueryEscape(params[k]))
	}
	paramStr := strings.Join(encodedPairs, "&")

	baseStr := "GET&" + url.QueryEscape(baseURL) + "&" + url.QueryEscape(paramStr)
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
		encodedPairs = append(encodedPairs, k+"="+url.QueryEscape(v))
	}

	req, err := http.NewRequest("GET", uri+"?"+strings.Join(encodedPairs, "&"), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

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
		encoded := k + "=" + url.QueryEscape(v)
		encodedPairs = append(encodedPairs, encoded)
	}

	reqURL := uri + "?" + strings.Join(encodedPairs, "&")
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

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
		return nil, fmt.Errorf("flickr api error %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}
